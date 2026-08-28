package cmd

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"cloister.io/internal/config"
	"cloister.io/internal/workspace/scan"
	"cloister.io/internal/workspace/source"
	"github.com/spf13/cobra"
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Discover and safely configure project workspaces",
}

var workspaceScanJSON bool
var workspaceShowJSON bool
var workspaceShowAllowStale bool
var workspaceApplyYes bool
var workspaceReviewFlags workspaceReviewOptions

type workspaceReviewOptions struct {
	Yes                   bool
	AcceptRecommendations bool
	ExcludeUnresolved     bool
	IncludeClass          []string
	ExcludeClass          []string
	IncludePath           []string
	ExcludePath           []string
	IncludeProject        []string
	ExcludeProject        []string
}

func (options workspaceReviewOptions) hasDecisionFlags() bool {
	return options.AcceptRecommendations || options.ExcludeUnresolved ||
		len(options.IncludeClass) > 0 || len(options.ExcludeClass) > 0 ||
		len(options.IncludePath) > 0 || len(options.ExcludePath) > 0 ||
		len(options.IncludeProject) > 0 || len(options.ExcludeProject) > 0
}

var workspaceScanCmd = &cobra.Command{
	Use:   "scan <profile|path>",
	Short: "Scan workspace metadata and save a reviewable proposal",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkspaceScan(cmd, args[0])
	},
}

var workspaceReviewCmd = &cobra.Command{
	Use:   "review <profile>",
	Short: "Review every undecided workspace finding",
	Long: `Review every undecided workspace finding.

Interactive review is the default. For scripts and agents, pass decision flags
instead of answering prompts:

  cloister workspace review <profile> --accept-recommendations --exclude-unresolved --yes
  cloister workspace review <profile> --include-class agent_config --exclude-class unknown_large --yes

--accept-recommendations applies scanner include/exclude advice only. Items
still marked review stay unresolved unless a class, path, or project flag
covers them, or --exclude-unresolved excludes them. Incomplete projects cannot
be included. --yes skips the final save confirmation.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkspaceReview(cmd, args[0])
	},
}

var workspaceShowCmd = &cobra.Command{
	Use:   "show <profile>",
	Short: "Show the saved workspace proposal",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkspaceShow(cmd, args[0])
	},
}

var workspaceApplyCmd = &cobra.Command{
	Use:   "apply <profile>",
	Short: "Apply a reviewed workspace proposal to local configuration",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkspaceApply(cmd, args[0])
	},
}

func init() {
	workspaceScanCmd.Flags().BoolVar(&workspaceScanJSON, "json", false, "Print only the portable proposal JSON")
	workspaceShowCmd.Flags().BoolVar(&workspaceShowJSON, "json", false, "Print only the portable proposal JSON")
	workspaceShowCmd.Flags().BoolVar(&workspaceShowAllowStale, "allow-stale", false, "Show saved state even when current configuration has changed")
	workspaceReviewCmd.Flags().BoolVarP(&workspaceReviewFlags.Yes, "yes", "y", false, "Save reviewed decisions without a confirmation prompt")
	workspaceReviewCmd.Flags().BoolVar(&workspaceReviewFlags.AcceptRecommendations, "accept-recommendations", false, "Apply scanner include/exclude recommendations without prompting")
	workspaceReviewCmd.Flags().BoolVar(&workspaceReviewFlags.ExcludeUnresolved, "exclude-unresolved", false, "Exclude remaining review items, including incomplete projects")
	workspaceReviewCmd.Flags().StringSliceVar(&workspaceReviewFlags.IncludeClass, "include-class", nil, "Include review findings of these classes (repeatable or comma-separated)")
	workspaceReviewCmd.Flags().StringSliceVar(&workspaceReviewFlags.ExcludeClass, "exclude-class", nil, "Exclude review findings of these classes (repeatable or comma-separated)")
	workspaceReviewCmd.Flags().StringSliceVar(&workspaceReviewFlags.IncludePath, "include-path", nil, "Include review findings whose path or basename matches a glob")
	workspaceReviewCmd.Flags().StringSliceVar(&workspaceReviewFlags.ExcludePath, "exclude-path", nil, "Exclude review findings whose path or basename matches a glob")
	workspaceReviewCmd.Flags().StringSliceVar(&workspaceReviewFlags.IncludeProject, "include-project", nil, "Include review repository candidates by id or portable path")
	workspaceReviewCmd.Flags().StringSliceVar(&workspaceReviewFlags.ExcludeProject, "exclude-project", nil, "Exclude review repository candidates by id or portable path")
	workspaceApplyCmd.Flags().BoolVarP(&workspaceApplyYes, "yes", "y", false, "Write the workspace config delta without a confirmation prompt")
	workspaceCmd.AddCommand(workspaceScanCmd, workspaceReviewCmd, workspaceShowCmd, workspaceApplyCmd)
	rootCmd.AddCommand(workspaceCmd)
}

type workspaceSelection struct {
	profileName string
	profile     *config.Profile
	startDir    string
	sourceRoot  string
	result      source.Result
}

func runWorkspaceScan(cmd *cobra.Command, target string) error {
	home, _, cfg, err := loadWorkspaceConfig()
	if err != nil {
		return err
	}
	selection, err := selectWorkspaceSource(target, home, cfg)
	if err != nil {
		return err
	}
	options := selection.result.ScanOptions()
	options.Generator = "cloister " + Version
	proposal, snapshot, err := scan.ScanWithSnapshot(options)
	if err != nil {
		return fmt.Errorf("scanning workspace: %w", err)
	}
	fingerprint, err := workspaceConfigFingerprint(selection.profile)
	if err != nil {
		return err
	}
	digest, err := scan.ProposalDigest(*proposal)
	if err != nil {
		return err
	}
	sourceFingerprint, err := workspaceSourceFingerprint(selection.result)
	if err != nil {
		return err
	}
	mappings, err := workspaceProjectMappings(selection.result)
	if err != nil {
		return err
	}
	state := scan.StateEnvelope{
		FormatVersion: scan.CurrentFormatVersion, Profile: selection.profileName,
		SourceRoot: selection.sourceRoot, ConfigFingerprint: fingerprint,
		SourceFingerprint: sourceFingerprint, ProposalDigest: digest,
		ContentFingerprint:  snapshot.ContentFingerprint,
		ProjectFingerprints: snapshot.ProjectFingerprints,
		ProjectMappings:     mappings, Reviewed: false, Proposal: *proposal,
	}
	statePath, err := workspaceStatePath(home, selection.profileName)
	if err != nil {
		return err
	}
	if err := scan.SaveState(statePath, state); err != nil {
		return fmt.Errorf("saving workspace scan: %w", err)
	}
	if workspaceScanJSON {
		data, marshalErr := scan.MarshalProposal(*proposal)
		if marshalErr != nil {
			return marshalErr
		}
		cmd.Println(string(data))
	} else {
		cmd.Printf("Saved workspace scan for profile %q with %d projects and %d findings.\n", selection.profileName, len(proposal.Projects), len(proposal.Findings))
		if selection.result.Adapter == scan.SourceAdapterRepository &&
			len(proposal.Projects) == 1 && proposal.Projects[0].Path == "." {
			cmd.Println("This source root contains a single repository. Consider mode: broker instead of mode: workspace.")
		}
		cmd.Printf("Review it with: cloister workspace review %s\n", selection.profileName)
	}
	return nil
}

func runWorkspaceReview(cmd *cobra.Command, profileName string) error {
	home, _, cfg, err := loadWorkspaceConfig()
	if err != nil {
		return err
	}
	state, path, err := loadFreshWorkspaceState(home, profileName, cfg)
	if err != nil {
		return err
	}
	proposal := state.Proposal
	if err := reviewProposalWith(&proposal, cmd.InOrStdin(), cmd.OutOrStdout(), workspaceReviewFlags); err != nil {
		return err
	}
	digest, err := scan.ProposalDigest(proposal)
	if err != nil {
		return err
	}
	state.Proposal = proposal
	state.ProposalDigest = digest
	state.Reviewed = true
	if err := scan.SaveState(path, state); err != nil {
		return fmt.Errorf("saving reviewed workspace state: %w", err)
	}
	cmd.Println("Reviewed workspace proposal saved.")
	return nil
}

func runWorkspaceShow(cmd *cobra.Command, profileName string) error {
	home, _, cfg, err := loadWorkspaceConfig()
	if err != nil {
		return err
	}
	state, _, err := loadFreshWorkspaceState(home, profileName, cfg)
	if err != nil {
		var staleError *workspaceStateStaleError
		if !workspaceShowAllowStale || !errors.As(err, &staleError) {
			return err
		}
		state, err = loadWorkspaceState(home, profileName)
		if err != nil {
			return err
		}
		cmd.Println("WARNING: saved workspace state does not match the current configuration and must be re-scanned before it can be applied.")
	}
	if workspaceShowJSON {
		data, marshalErr := scan.MarshalProposal(state.Proposal)
		if marshalErr != nil {
			return marshalErr
		}
		cmd.Println(string(data))
		return nil
	}
	cmd.Printf("Profile: %s\nReviewed: %t\nProjects: %d\nFindings: %d\n", profileName, state.Reviewed, len(state.Proposal.Projects), len(state.Proposal.Findings))
	printProposalSections(cmd.OutOrStdout(), state.Proposal, false)
	return nil
}

func runWorkspaceApply(cmd *cobra.Command, profileName string) error {
	home, configPath, cfg, err := loadWorkspaceConfig()
	if err != nil {
		return err
	}
	state, _, err := loadFreshWorkspaceState(home, profileName, cfg)
	if err != nil {
		return err
	}
	if !state.Reviewed {
		return fmt.Errorf("workspace proposal has not been reviewed")
	}
	for _, project := range state.Proposal.Projects {
		if project.IncompleteScan && project.Decision == scan.DecisionInclude {
			return incompleteProjectApplyError(project.ID)
		}
		if project.Decision == scan.DecisionReview {
			return fmt.Errorf("workspace proposal has unresolved project decisions")
		}
	}
	for _, finding := range state.Proposal.Findings {
		if finding.Decision == scan.DecisionReview {
			return fmt.Errorf("workspace proposal has unresolved review decisions")
		}
	}
	if err := validatePortableProjectMappings(state.SourceRoot, state.Proposal, state.ProjectMappings); err != nil {
		return err
	}
	next, err := buildAppliedWorkspace(state.SourceRoot, state.Proposal)
	if err != nil {
		return err
	}
	return saveAppliedConfig(configPath, cfg, profileName, next, cmd.InOrStdin(), cmd.OutOrStdout(), workspaceApplyYes)
}

func loadWorkspaceConfig() (string, string, *config.Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", nil, err
	}
	path := filepath.Join(home, ".cloister", "config.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		return "", "", nil, fmt.Errorf("loading config: %w", err)
	}
	return home, path, cfg, nil
}

func selectWorkspaceSource(target, home string, cfg *config.Config) (workspaceSelection, error) {
	var profileName, sourceBase string
	profile, isProfile := cfg.Profiles[target]
	if isProfile {
		profileName = target
		sourceBase = profile.StartDir
		if sourceBase == "" {
			sourceBase = config.DefaultStartDir
		}
		resolved, err := config.ResolveWorkspaceDir(sourceBase, home)
		if err != nil {
			return workspaceSelection{}, err
		}
		sourceBase = resolved
	} else {
		resolvedProfile, resolvedScope, err := resolveWorkspaceProfilePath(target, home, cfg.Profiles)
		if err != nil {
			return workspaceSelection{}, err
		}
		profileName, sourceBase = resolvedProfile, resolvedScope
		profile = cfg.Profiles[profileName]
	}
	if profile.Workspace.Root != "" {
		resolved, err := config.ResolveWorkspaceDir(profile.Workspace.Root, home)
		if err != nil {
			return workspaceSelection{}, err
		}
		sourceBase = resolved
	}
	canonical, err := canonicalWorkspaceRoot(sourceBase)
	if err != nil {
		return workspaceSelection{}, err
	}
	result, err := loadWorkspaceSource(canonical, home, profile.Workspace)
	if err != nil {
		return workspaceSelection{}, err
	}
	return workspaceSelection{profileName: profileName, profile: profile, startDir: sourceBase, sourceRoot: result.Root, result: result}, nil
}

func resolveWorkspaceProfilePath(target, home string, profiles map[string]*config.Profile) (string, string, error) {
	canonicalTarget, err := canonicalWorkspaceRoot(target)
	if err != nil {
		return "", "", err
	}
	type match struct{ name, root string }
	var matches []match
	for name, profile := range profiles {
		if profile == nil {
			continue
		}
		configured := profile.Workspace.Root
		if configured == "" {
			configured = profile.StartDir
		}
		if configured == "" {
			configured = config.DefaultStartDir
		}
		resolved, resolveErr := config.ResolveWorkspaceDir(configured, home)
		if resolveErr != nil {
			continue
		}
		root, rootErr := canonicalWorkspaceRoot(resolved)
		if rootErr != nil {
			continue
		}
		relative, relErr := filepath.Rel(root, canonicalTarget)
		if relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			matches = append(matches, match{name, root})
		}
	}
	if len(matches) == 0 {
		return "", "", fmt.Errorf("project path is not within any configured workspace source root")
	}
	sort.Slice(matches, func(i, j int) bool {
		if len(matches[i].root) != len(matches[j].root) {
			return len(matches[i].root) > len(matches[j].root)
		}
		return matches[i].name < matches[j].name
	})
	if len(matches) > 1 && len(matches[0].root) == len(matches[1].root) {
		return "", "", fmt.Errorf("project path matches equally specific workspace profiles")
	}
	return matches[0].name, matches[0].root, nil
}

func loadWorkspaceSource(root, _ string, workspaceConfig config.WorkspaceConfig) (source.Result, error) {
	manifestPath := filepath.Join(root, "manifest", "projects.json")
	info, err := os.Lstat(manifestPath)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return source.Result{}, fmt.Errorf("workspace manifest metadata must be a regular file")
		}
		manifestDirInfo, statErr := os.Lstat(filepath.Dir(manifestPath))
		if statErr != nil || !manifestDirInfo.IsDir() || manifestDirInfo.Mode()&os.ModeSymlink != 0 {
			return source.Result{}, fmt.Errorf("workspace manifest metadata directory must be a real directory")
		}
		return source.NewManifest(source.ManifestOptions{
			Root: root, LookupEnv: os.LookupEnv,
			ApprovedExternalRoots: workspaceEnvironmentRoots(),
		}).Load()
	}
	if !errors.Is(err, os.ErrNotExist) {
		return source.Result{}, sanitizedWorkspaceError("checking workspace manifest metadata at \"manifest/projects.json\" failed", err)
	}
	return source.NewRepositoryCatalog(source.RepositoryOptions{
		Root: root, Config: workspaceConfig,
	}).Load()
}

func workspaceEnvironmentRoots() []string {
	var roots []string
	if projects, ok := os.LookupEnv("WORKSPACE_PROJECTS_DIR"); ok && filepath.IsAbs(projects) {
		if info, err := os.Stat(projects); err == nil && info.IsDir() {
			roots = append(roots, projects)
		}
	}
	if workspaceRoot, ok := os.LookupEnv("WORKSPACE_ROOT"); ok && filepath.IsAbs(workspaceRoot) {
		projects := filepath.Join(workspaceRoot, "projects")
		if info, err := os.Stat(projects); err == nil && info.IsDir() {
			roots = append(roots, projects)
		}
	}
	return roots
}

func canonicalWorkspaceRoot(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", sanitizedWorkspaceError("resolving workspace source root failed", err)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("workspace source root must be a directory")
	}
	return absolute, nil
}

type sanitizedWorkspacePathError struct {
	message string
	cause   error
}

func (err *sanitizedWorkspacePathError) Error() string {
	return err.message
}

func (err *sanitizedWorkspacePathError) Unwrap() error {
	return err.cause
}

func sanitizedWorkspaceError(message string, cause error) error {
	var pathError *fs.PathError
	if errors.As(cause, &pathError) {
		cause = pathError.Err
	}
	return &sanitizedWorkspacePathError{message: message, cause: cause}
}

func workspaceStatePath(home, profile string) (string, error) {
	if profile == "" || filepath.Base(profile) != profile || profile == "." || profile == ".." || strings.ContainsAny(profile, `/\`+"\x00") {
		return "", fmt.Errorf("unsafe workspace profile name")
	}
	return filepath.Join(home, ".cloister", "state", "workspace-scans", profile+".json"), nil
}

func workspaceConfigFingerprint(profile *config.Profile) (string, error) {
	if profile == nil {
		return "", fmt.Errorf("profile is required")
	}
	relevant := struct {
		StartDir  string                 `json:"startDir"`
		Workspace config.WorkspaceConfig `json:"workspace"`
	}{profile.StartDir, profile.Workspace}
	data, err := json.Marshal(relevant)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func workspaceSourceFingerprint(result source.Result) (string, error) {
	data, err := json.Marshal(struct {
		Root          string
		Adapter       scan.SourceAdapter
		Projects      []scan.ProjectDescriptor
		Policy        scan.Policy
		ApprovedRoots []string
		Metadata      string
	}{
		result.Root, result.Adapter, result.Projects, result.Policy,
		result.ApprovedProjectRoots, result.MetadataDigest,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func workspaceProjectMappings(result source.Result) ([]scan.ProjectMapping, error) {
	mappings := make([]scan.ProjectMapping, 0, len(result.Projects))
	for _, descriptor := range result.Projects {
		root := descriptor.Root
		if root == "" {
			root = filepath.Join(result.Root, filepath.FromSlash(descriptor.Path))
		}
		canonical, err := canonicalWorkspaceRoot(root)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, scan.ProjectMapping{
			ProjectID: descriptor.ID, PortablePath: descriptor.Path, PhysicalRoot: canonical,
		})
	}
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].ProjectID < mappings[j].ProjectID })
	return mappings, nil
}

func loadFreshWorkspaceState(home, profileName string, cfg *config.Config) (scan.StateEnvelope, string, error) {
	profile := cfg.Profiles[profileName]
	if profile == nil {
		return scan.StateEnvelope{}, "", newWorkspaceStateStaleError(
			fmt.Sprintf("workspace scan is stale because profile %q is no longer configured", profileName),
		)
	}
	path, err := workspaceStatePath(home, profileName)
	if err != nil {
		return scan.StateEnvelope{}, "", err
	}
	state, err := scan.LoadState(path)
	if err != nil {
		return scan.StateEnvelope{}, "", err
	}
	if state.Profile != profileName {
		return scan.StateEnvelope{}, "", fmt.Errorf("state profile does not match requested profile")
	}
	fingerprint, err := workspaceConfigFingerprint(profile)
	if err != nil {
		return scan.StateEnvelope{}, "", err
	}
	if fingerprint != state.ConfigFingerprint {
		return scan.StateEnvelope{}, "", newWorkspaceStateStaleError("workspace scan is stale because relevant profile configuration changed")
	}
	selection, err := selectWorkspaceSource(profileName, home, cfg)
	if err != nil {
		return scan.StateEnvelope{}, "", fmt.Errorf("reloading workspace source: %w", err)
	}
	sourceFingerprint, err := workspaceSourceFingerprint(selection.result)
	if err != nil {
		return scan.StateEnvelope{}, "", err
	}
	if sourceFingerprint != state.SourceFingerprint || selection.sourceRoot != state.SourceRoot {
		return scan.StateEnvelope{}, "", newWorkspaceStateStaleError("workspace scan is stale because source catalog metadata changed")
	}
	freshMappings, err := workspaceProjectMappings(selection.result)
	if err != nil {
		return scan.StateEnvelope{}, "", err
	}
	if !sameProjectMappings(freshMappings, state.ProjectMappings) {
		return scan.StateEnvelope{}, "", newWorkspaceStateStaleError("workspace scan is stale or tampered because project mappings changed")
	}
	contentSnapshot, err := scan.ContentSnapshot(selection.result.ScanOptions())
	if err != nil {
		return scan.StateEnvelope{}, "", fmt.Errorf("checking workspace project tree: %w", err)
	}
	if contentSnapshot.ContentFingerprint != state.ContentFingerprint {
		changed := changedProjectSummary(
			state.Proposal.Projects,
			state.ProjectFingerprints,
			contentSnapshot.ProjectFingerprints,
			5,
		)
		message := "workspace scan is stale because the project tree changed"
		if changed != "" {
			message += " in " + changed
		}
		return scan.StateEnvelope{}, "", newWorkspaceStateStaleError(message + "; re-run cloister workspace scan")
	}
	return state, path, nil
}

func changedProjectSummary(
	projects []scan.Project,
	saved map[string]string,
	current map[string]string,
	limit int,
) string {
	var paths []string
	for _, project := range projects {
		if saved[project.ID] != current[project.ID] {
			paths = append(paths, project.Path)
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 || limit <= 0 {
		return ""
	}
	remainder := len(paths) - limit
	if remainder > 0 {
		paths = paths[:limit]
	}
	summary := strings.Join(paths, ", ")
	if remainder > 0 {
		summary += fmt.Sprintf(", and %d more", remainder)
	}
	return summary
}

type workspaceStateStaleError struct {
	message string
}

func (err *workspaceStateStaleError) Error() string {
	return err.message
}

func newWorkspaceStateStaleError(message string) error {
	return &workspaceStateStaleError{message: message}
}

func loadWorkspaceState(home, profileName string) (scan.StateEnvelope, error) {
	path, err := workspaceStatePath(home, profileName)
	if err != nil {
		return scan.StateEnvelope{}, err
	}
	state, err := scan.LoadState(path)
	if err != nil {
		return scan.StateEnvelope{}, err
	}
	if state.Profile != profileName {
		return scan.StateEnvelope{}, fmt.Errorf("state profile does not match requested profile")
	}
	return state, nil
}

func sameProjectMappings(left, right []scan.ProjectMapping) bool {
	if len(left) != len(right) {
		return false
	}
	orderedLeft := append([]scan.ProjectMapping(nil), left...)
	orderedRight := append([]scan.ProjectMapping(nil), right...)
	sort.Slice(orderedLeft, func(i, j int) bool { return orderedLeft[i].ProjectID < orderedLeft[j].ProjectID })
	sort.Slice(orderedRight, func(i, j int) bool { return orderedRight[i].ProjectID < orderedRight[j].ProjectID })
	return reflect.DeepEqual(orderedLeft, orderedRight)
}

type reviewSection struct {
	title string
	match func(scan.FindingClass) bool
}

var workspaceReviewSections = []reviewSection{
	{"Env and secrets", func(class scan.FindingClass) bool {
		return class == scan.ClassSecretLocalConfig || class == scan.ClassHostPrivateAgentState
	}},
	{"Dependencies", func(class scan.FindingClass) bool { return class == scan.ClassDependency }},
	{"Artifacts", func(class scan.FindingClass) bool { return class == scan.ClassGeneratedArtifact }},
	{"Local databases, dumps, and scripts", func(class scan.FindingClass) bool {
		return class == scan.ClassDatabase || class == scan.ClassDatabaseDump || class == scan.ClassDatabaseScript
	}},
	{"Application commands and services", func(class scan.FindingClass) bool {
		return class == scan.ClassApplicationManifest || class == scan.ClassServiceManifest
	}},
	{"Agent config", func(class scan.FindingClass) bool { return class == scan.ClassAgentConfig }},
	{"Unknown large files", func(class scan.FindingClass) bool { return class == scan.ClassUnknownLarge }},
	{"Other source and local inputs", func(class scan.FindingClass) bool { return class == scan.ClassSource }},
}

func reviewProposal(proposal *scan.Proposal, input io.Reader, output io.Writer) error {
	return reviewProposalWith(proposal, input, output, workspaceReviewOptions{})
}

func reviewProposalWith(proposal *scan.Proposal, input io.Reader, output io.Writer, options workspaceReviewOptions) error {
	reviewed := *proposal
	reviewed.Projects = append([]scan.Project(nil), proposal.Projects...)
	reviewed.Findings = append([]scan.Finding(nil), proposal.Findings...)
	reviewed.Exclusions = append([]scan.Exclusion(nil), proposal.Exclusions...)
	if options.hasDecisionFlags() {
		if err := applyNonInteractiveReview(&reviewed, options); err != nil {
			return err
		}
		excludeNestedSelectedProjects(&reviewed, output)
		if err := rejectUnresolvedReview(reviewed); err != nil {
			return err
		}
		included, excluded := decisionCounts(reviewed.Findings)
		fmt.Fprintf(output, "Summary: %d included, %d excluded.\n", included, excluded)
		if !options.Yes {
			return fmt.Errorf("pass --yes to save reviewed decisions non-interactively")
		}
		scan.RebuildExclusions(&reviewed)
		*proposal = reviewed
		return nil
	}
	reader := bufio.NewReader(input)
	printProposalSections(output, *proposal, true)
	for projectIndex := range reviewed.Projects {
		project := &reviewed.Projects[projectIndex]
		if project.Decision != scan.DecisionReview {
			continue
		}
		if project.IncompleteScan {
			fmt.Fprintf(
				output,
				"Repository projects: %s (%s) [e=exclude, or cancel and re-scan]: ",
				project.Path,
				project.ScanIssue,
			)
			answer, err := readAnswer(reader)
			if err != nil {
				return fmt.Errorf("review aborted before all decisions were resolved: %w", err)
			}
			if answer != "e" && answer != "exclude" {
				return fmt.Errorf(
					"project %q has an incomplete scan; exclude it or narrow it with per-project ignores and re-scan",
					project.ID,
				)
			}
			project.Decision = scan.DecisionExclude
			continue
		}
		fmt.Fprintf(
			output,
			"Repository projects: %s (%s) [i=include, e=exclude]: ",
			project.Path,
			project.Reason,
		)
		answer, err := readAnswer(reader)
		if err != nil {
			return fmt.Errorf("review aborted before all decisions were resolved: %w", err)
		}
		switch answer {
		case "i", "include":
			project.Decision = scan.DecisionInclude
		case "e", "exclude":
			project.Decision = scan.DecisionExclude
		default:
			return fmt.Errorf("review aborted: expected include or exclude")
		}
	}
	for _, section := range workspaceReviewSections {
		var unresolved []int
		for findingIndex := range reviewed.Findings {
			finding := reviewed.Findings[findingIndex]
			if section.match(finding.Class) && finding.Decision == scan.DecisionReview {
				unresolved = append(unresolved, findingIndex)
			}
		}
		for unresolvedIndex, findingIndex := range unresolved {
			finding := &reviewed.Findings[findingIndex]
			remaining := unresolved[unresolvedIndex:]
			if len(remaining) > 1 {
				fmt.Fprintf(
					output,
					"%s: %s/%s [i=include, e=exclude, ia=include-all, ea=exclude-all for current section]: ",
					section.title, finding.ProjectID, finding.Path,
				)
			} else {
				fmt.Fprintf(output, "%s: %s/%s [i=include, e=exclude]: ", section.title, finding.ProjectID, finding.Path)
			}
			answer, err := readAnswer(reader)
			if err != nil {
				return fmt.Errorf("review aborted before all decisions were resolved: %w", err)
			}
			switch answer {
			case "i", "include":
				finding.Decision = scan.DecisionInclude
			case "e", "exclude":
				finding.Decision = scan.DecisionExclude
			case "ia", "include-all":
				if len(remaining) < 2 {
					return fmt.Errorf("review aborted: bulk decision requires multiple unresolved findings in the current section")
				}
				setReviewDecisions(reviewed.Findings, remaining, scan.DecisionInclude)
			case "ea", "exclude-all":
				if len(remaining) < 2 {
					return fmt.Errorf("review aborted: bulk decision requires multiple unresolved findings in the current section")
				}
				setReviewDecisions(reviewed.Findings, remaining, scan.DecisionExclude)
			default:
				return fmt.Errorf("review aborted: expected include, exclude, include-all, or exclude-all")
			}
			if answer == "ia" || answer == "include-all" || answer == "ea" || answer == "exclude-all" {
				break
			}
		}
	}
	excludeNestedSelectedProjects(&reviewed, output)
	for _, finding := range reviewed.Findings {
		if finding.Decision == scan.DecisionReview {
			return fmt.Errorf("review has unresolved decisions")
		}
	}
	for _, project := range reviewed.Projects {
		if project.Decision == scan.DecisionReview {
			return fmt.Errorf("review has unresolved project decisions")
		}
	}
	included, excluded := decisionCounts(reviewed.Findings)
	fmt.Fprintf(output, "Summary: %d included, %d excluded. Save reviewed decisions? [y/N]: ", included, excluded)
	answer, err := readAnswer(reader)
	if err != nil || (answer != "y" && answer != "yes") {
		return fmt.Errorf("review not saved")
	}
	scan.RebuildExclusions(&reviewed)
	*proposal = reviewed
	return nil
}

func setReviewDecisions(findings []scan.Finding, indices []int, decision scan.Decision) {
	for _, index := range indices {
		findings[index].Decision = decision
	}
}

func applyNonInteractiveReview(proposal *scan.Proposal, options workspaceReviewOptions) error {
	includeClasses, err := parseReviewClasses(options.IncludeClass)
	if err != nil {
		return err
	}
	excludeClasses, err := parseReviewClasses(options.ExcludeClass)
	if err != nil {
		return err
	}
	for i := range proposal.Projects {
		project := &proposal.Projects[i]
		if project.Decision != scan.DecisionReview {
			continue
		}
		switch {
		case reviewNameMatches(options.ExcludeProject, project.ID, project.Path):
			project.Decision = scan.DecisionExclude
		case reviewNameMatches(options.IncludeProject, project.ID, project.Path):
			if project.IncompleteScan {
				return incompleteProjectApplyError(project.ID)
			}
			project.Decision = scan.DecisionInclude
		case options.AcceptRecommendations && project.Recommendation == scan.RecommendationInclude:
			if project.IncompleteScan {
				return incompleteProjectApplyError(project.ID)
			}
			project.Decision = scan.DecisionInclude
		case options.AcceptRecommendations && project.Recommendation == scan.RecommendationExclude:
			project.Decision = scan.DecisionExclude
		case options.ExcludeUnresolved:
			project.Decision = scan.DecisionExclude
		}
	}
	for i := range proposal.Findings {
		finding := &proposal.Findings[i]
		if finding.Decision != scan.DecisionReview {
			continue
		}
		switch {
		case reviewPathMatchesAny(options.ExcludePath, finding.Path):
			finding.Decision = scan.DecisionExclude
		case reviewPathMatchesAny(options.IncludePath, finding.Path):
			finding.Decision = scan.DecisionInclude
		case classSelected(excludeClasses, finding.Class):
			finding.Decision = scan.DecisionExclude
		case classSelected(includeClasses, finding.Class):
			finding.Decision = scan.DecisionInclude
		case options.AcceptRecommendations && finding.Recommendation == scan.RecommendationInclude:
			finding.Decision = scan.DecisionInclude
		case options.AcceptRecommendations && finding.Recommendation == scan.RecommendationExclude:
			finding.Decision = scan.DecisionExclude
		case options.ExcludeUnresolved:
			finding.Decision = scan.DecisionExclude
		}
	}
	return nil
}

func rejectUnresolvedReview(proposal scan.Proposal) error {
	var projects, findings int
	for _, project := range proposal.Projects {
		if project.Decision == scan.DecisionReview {
			projects++
		}
	}
	for _, finding := range proposal.Findings {
		if finding.Decision == scan.DecisionReview {
			findings++
		}
	}
	if projects == 0 && findings == 0 {
		return nil
	}
	return fmt.Errorf(
		"workspace review has %d unresolved project decisions and %d unresolved findings; pass --include-class/--exclude-class, --include-path/--exclude-path, --include-project/--exclude-project, or --exclude-unresolved",
		projects,
		findings,
	)
}

func excludeNestedSelectedProjects(proposal *scan.Proposal, output io.Writer) {
	for childIndex := range proposal.Projects {
		child := &proposal.Projects[childIndex]
		if child.Decision != scan.DecisionInclude {
			continue
		}
		for _, parent := range proposal.Projects {
			if parent.Decision != scan.DecisionInclude || parent.Path == child.Path {
				continue
			}
			if !projectPathContains(parent.Path, child.Path) {
				continue
			}
			child.Decision = scan.DecisionExclude
			fmt.Fprintf(output, "Excluded nested repository %s because %s is included.\n", child.Path, parent.Path)
			break
		}
	}
}

func parseReviewClasses(values []string) ([]scan.FindingClass, error) {
	classes := make([]scan.FindingClass, 0, len(values))
	for _, value := range values {
		class := scan.FindingClass(strings.TrimSpace(value))
		if class == "" {
			continue
		}
		if !knownFindingClass(class) {
			return nil, fmt.Errorf("unknown finding class %q", value)
		}
		classes = append(classes, class)
	}
	return classes, nil
}

func knownFindingClass(class scan.FindingClass) bool {
	switch class {
	case scan.ClassSource, scan.ClassSecretLocalConfig, scan.ClassHostPrivateAgentState,
		scan.ClassDependency, scan.ClassGeneratedArtifact, scan.ClassDatabase,
		scan.ClassDatabaseDump, scan.ClassDatabaseScript, scan.ClassApplicationManifest,
		scan.ClassServiceManifest, scan.ClassAgentConfig, scan.ClassUnknownLarge:
		return true
	default:
		return false
	}
}

func classSelected(classes []scan.FindingClass, class scan.FindingClass) bool {
	for _, candidate := range classes {
		if candidate == class {
			return true
		}
	}
	return false
}

func reviewNameMatches(names []string, id, portablePath string) bool {
	for _, name := range names {
		if name == id || name == portablePath {
			return true
		}
	}
	return false
}

func reviewPathMatchesAny(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if reviewPathMatches(pattern, value) {
			return true
		}
	}
	return false
}

func reviewPathMatches(pattern, value string) bool {
	if pattern == value {
		return true
	}
	if matched, err := path.Match(pattern, value); err == nil && matched {
		return true
	}
	if matched, err := path.Match(pattern, path.Base(value)); err == nil && matched {
		return true
	}
	return false
}

func printProposalSections(output io.Writer, proposal scan.Proposal, includeEmpty bool) {
	fmt.Fprintf(output, "\nRepository projects (%d)\n", len(proposal.Projects))
	for _, project := range proposal.Projects {
		if project.Decision == scan.DecisionInclude {
			continue
		}
		reason := project.Reason
		if project.IncompleteScan {
			reason = project.ScanIssue
		}
		fmt.Fprintf(output, "  %s  %s  %s\n", project.Path, project.Decision, reason)
	}
	for _, section := range workspaceReviewSections {
		var findings []scan.Finding
		for _, finding := range proposal.Findings {
			if section.match(finding.Class) {
				findings = append(findings, finding)
			}
		}
		if len(findings) == 0 && !includeEmpty {
			continue
		}
		fmt.Fprintf(output, "\n%s (%d)\n", section.title, len(findings))
		for _, finding := range findings {
			if finding.Decision == scan.DecisionInclude {
				continue
			}
			fmt.Fprintf(output, "  %s/%s  %s  %s\n", finding.ProjectID, finding.Path, finding.Class, finding.Decision)
		}
		if section.title == "Application commands and services" {
			for _, command := range proposal.Commands {
				fmt.Fprintf(output, "  %s/%s  command %s\n", command.ProjectID, command.Path, command.Name)
			}
			for _, service := range proposal.Services {
				fmt.Fprintf(output, "  %s/%s  service %s\n", service.ProjectID, service.Path, service.Name)
			}
		}
	}
}

func readAnswer(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(line)), nil
}

func decisionCounts(findings []scan.Finding) (int, int) {
	var included, excluded int
	for _, finding := range findings {
		if finding.Decision == scan.DecisionInclude {
			included++
		} else if finding.Decision == scan.DecisionExclude {
			excluded++
		}
	}
	return included, excluded
}

func buildAppliedWorkspace(root string, proposal scan.Proposal) (config.WorkspaceConfig, error) {
	projectByID := make(map[string]string, len(proposal.Projects))
	selectors := make([]string, 0, len(proposal.Projects))
	for _, project := range proposal.Projects {
		if project.Decision == scan.DecisionReview {
			return config.WorkspaceConfig{}, fmt.Errorf("workspace proposal has unresolved project decisions")
		}
		if project.Decision != scan.DecisionInclude {
			continue
		}
		if project.IncompleteScan {
			return config.WorkspaceConfig{}, incompleteProjectApplyError(project.ID)
		}
		projectByID[project.ID] = project.Path
		selectors = append(selectors, project.Path)
	}
	sort.Strings(selectors)
	if len(selectors) == 0 {
		return config.WorkspaceConfig{}, fmt.Errorf("workspace proposal selects no projects")
	}
	for parentIndex, parent := range selectors {
		for _, child := range selectors[parentIndex+1:] {
			if projectPathContains(parent, child) {
				return config.WorkspaceConfig{}, fmt.Errorf(
					"selected projects %q and %q overlap; keep exactly one of them",
					parent,
					child,
				)
			}
		}
	}
	projectIgnore := make(map[string][]string, len(proposal.Policy.ProjectIgnore))
	for project, patterns := range proposal.Policy.ProjectIgnore {
		if containsString(selectors, project) {
			projectIgnore[project] = append([]string(nil), patterns...)
		}
	}
	for _, parent := range selectors {
		for _, candidate := range proposal.Projects {
			if !projectPathContains(parent, candidate.Path) {
				continue
			}
			relative := strings.TrimPrefix(candidate.Path, parent+"/")
			if parent == "." {
				relative = candidate.Path
			}
			projectIgnore[parent] = appendUnique(projectIgnore[parent], relative+"/")
		}
	}
	for _, finding := range proposal.Findings {
		if finding.Decision == scan.DecisionReview {
			return config.WorkspaceConfig{}, fmt.Errorf("workspace proposal has unresolved review decisions")
		}
		if finding.Decision != scan.DecisionExclude {
			continue
		}
		project, selected := projectByID[finding.ProjectID]
		if !selected {
			continue
		}
		pattern := finding.Path
		if finding.Directory && !strings.HasSuffix(pattern, "/") {
			pattern += "/"
		}
		projectIgnore[project] = appendUnique(projectIgnore[project], pattern)
	}
	for project := range projectIgnore {
		sort.Strings(projectIgnore[project])
	}
	return config.WorkspaceConfig{
		Mode: config.WorkspaceModeWorkspace, Root: root, Selectors: selectors,
		Ignore: append([]string(nil), proposal.Policy.Ignore...), ProjectIgnore: projectIgnore,
		MaxEntryCount:      uint64(proposal.Policy.MaxEntriesPerProject),
		MaxStagingFileSize: proposal.Policy.MaxStagingFileSize,
	}, nil
}

func incompleteProjectApplyError(projectID string) error {
	return fmt.Errorf(
		"project %q cannot be included because its scan is incomplete; exclude it or narrow it with per-project ignores and re-scan",
		projectID,
	)
}

func projectPathContains(parent, child string) bool {
	if parent == "." {
		return child != "."
	}
	return strings.HasPrefix(child, parent+"/")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func validatePortableProjectMappings(root string, proposal scan.Proposal, mappings []scan.ProjectMapping) error {
	byID := make(map[string]scan.ProjectMapping, len(mappings))
	for _, mapping := range mappings {
		byID[mapping.ProjectID] = mapping
	}
	for _, project := range proposal.Projects {
		mapping, ok := byID[project.ID]
		expected := filepath.Join(root, filepath.FromSlash(project.Path))
		canonical, err := canonicalWorkspaceRoot(expected)
		if !ok || err != nil || mapping.PortablePath != project.Path || mapping.PhysicalRoot != canonical {
			return fmt.Errorf("project %q uses an external or stale source mapping that local workspace selectors cannot represent safely", project.ID)
		}
	}
	return nil
}

func saveAppliedConfig(path string, cfg *config.Config, profileName string, next config.WorkspaceConfig, input io.Reader, output io.Writer, skipConfirm bool) error {
	profile := cfg.Profiles[profileName]
	if profile == nil {
		return fmt.Errorf("profile %q is not configured", profileName)
	}
	fmt.Fprintln(output, "Workspace config field delta:")
	printWorkspaceDelta(output, profile.Workspace, next)
	if !skipConfirm {
		fmt.Fprint(output, "Apply this exact change? [y/N]: ")
		answer, err := readAnswer(bufio.NewReader(input))
		if err != nil || (answer != "y" && answer != "yes") {
			return fmt.Errorf("workspace apply cancelled")
		}
	}
	profile.Workspace = next
	if err := config.Save(path, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Fprintln(output, "Workspace configuration saved.")
	return nil
}

func printWorkspaceDelta(output io.Writer, before, after config.WorkspaceConfig) {
	fields := []struct {
		name   string
		before any
		after  any
	}{
		{"mode", before.Mode, after.Mode},
		{"root", before.Root, after.Root},
		{"selectors (pinned approved projects)", before.Selectors, after.Selectors},
		{"ignore", before.Ignore, after.Ignore},
		{"project_ignore", before.ProjectIgnore, after.ProjectIgnore},
		{"max_entry_count", before.MaxEntryCount, after.MaxEntryCount},
		{"max_staging_file_size", before.MaxStagingFileSize, after.MaxStagingFileSize},
	}
	for _, field := range fields {
		if reflect.DeepEqual(field.before, field.after) {
			continue
		}
		beforeJSON, _ := json.Marshal(field.before)
		afterJSON, _ := json.Marshal(field.after)
		fmt.Fprintf(output, "  %s: %s -> %s\n", field.name, beforeJSON, afterJSON)
	}
}
