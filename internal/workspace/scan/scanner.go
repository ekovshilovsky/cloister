package scan

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	brokerignore "cloister.io/internal/broker/ignore"
)

const (
	DefaultMaxEntriesPerProject int64 = 100_000
	DefaultMaxBytesPerProject   int64 = 4 << 30
	DefaultLargeFileBytes       int64 = 50 << 20
	maxManifestBytes                  = 1 << 20
)

type ProjectDescriptor struct {
	ID                 string
	Path               string
	Kind               ProjectKind
	NestedRepositories int
	Reason             string
	Recommendation     Recommendation
	Decision           Decision
	// Root is the optional host directory backing portable Path. It is accepted
	// only when contained by an explicitly approved project root.
	Root string
}

type OpenFileFunc func(path string) (io.ReadCloser, error)

type Options struct {
	SourceRoot           string
	Projects             []ProjectDescriptor
	SourceAdapter        SourceAdapter
	Generator            string
	CreatedAt            time.Time
	MaxEntriesPerProject int64
	MaxBytesPerProject   int64
	LargeFileBytes       int64
	OpenFile             OpenFileFunc
	Policy               Policy
	ApprovedProjectRoots []string
}

type Snapshot struct {
	ContentFingerprint  string
	ProjectFingerprints map[string]string
}

type LimitKind string

const (
	LimitEntries LimitKind = "entries"
	LimitBytes   LimitKind = "bytes"
)

type LimitError struct {
	ProjectID string
	Kind      LimitKind
	Limit     int64
	Observed  int64
	Issue     string
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("project %q scan is incomplete: %s", e.ProjectID, e.Issue)
}

type classification struct {
	class          FindingClass
	reason         string
	recommendation Recommendation
	decision       Decision
	prune          bool
}

type entryMetadata struct {
	relativePath   string
	directory      bool
	size           int64
	largeFileBytes int64
}

type sanitizedScanError struct {
	message string
	cause   error
}

func (e *sanitizedScanError) Error() string {
	return e.message
}

func (e *sanitizedScanError) Unwrap() error {
	return e.cause
}

func sanitizedError(message string, cause error) error {
	var pathError *fs.PathError
	if errors.As(cause, &pathError) {
		cause = pathError.Err
	}
	return &sanitizedScanError{message: message, cause: cause}
}

func Scan(options Options) (*Proposal, error) {
	proposal, _, err := ScanWithSnapshot(options)
	return proposal, err
}

func ScanWithSnapshot(options Options) (*Proposal, Snapshot, error) {
	proposal, snapshot, err := scanWithSnapshot(options, true)
	return proposal, snapshot, err
}

func ContentFingerprint(options Options) (string, error) {
	snapshot, err := ContentSnapshot(options)
	if err != nil {
		return "", err
	}
	return snapshot.ContentFingerprint, nil
}

func ContentSnapshot(options Options) (Snapshot, error) {
	_, snapshot, err := scanWithSnapshot(options, false)
	return snapshot, err
}

func scanWithSnapshot(options Options, buildProposal bool) (*Proposal, Snapshot, error) {
	options = withDefaults(options)
	if !validSourceAdapter(options.SourceAdapter) {
		return nil, Snapshot{}, fmt.Errorf("unsupported source adapter %q", options.SourceAdapter)
	}
	sourceRoot, err := canonicalSourceRoot(options.SourceRoot)
	if err != nil {
		return nil, Snapshot{}, err
	}
	projects, projectPaths, err := validateProjects(
		sourceRoot,
		options.Projects,
		options.ApprovedProjectRoots,
		options.SourceAdapter,
	)
	if err != nil {
		return nil, Snapshot{}, err
	}

	var proposal *Proposal
	if buildProposal {
		proposal = &Proposal{
			SchemaVersion: CurrentSchemaVersion,
			CreatedAt:     options.CreatedAt,
			Generator:     options.Generator,
			Source:        SourceMetadata{Root: ".", Adapter: options.SourceAdapter},
			Projects:      projects,
			Findings:      []Finding{},
			Runtimes:      []Runtime{},
			Commands:      []Command{},
			Services:      []Service{},
			Policy: Policy{
				Selectors:            cloneSlice(options.Policy.Selectors),
				Ignore:               cloneSlice(options.Policy.Ignore),
				ProjectIgnore:        cloneStringSliceMap(options.Policy.ProjectIgnore),
				MaxStagingFileSize:   options.Policy.MaxStagingFileSize,
				MaxEntriesPerProject: options.MaxEntriesPerProject,
				MaxBytesPerProject:   options.MaxBytesPerProject,
				PrunePatterns:        cloneSlice(options.Policy.PrunePatterns),
			},
			Exclusions:               []Exclusion{},
			CloudReadiness:           CloudReadinessLocalOnly,
			UnansweredCloudQuestions: []string{},
		}
	}

	fingerprint := sha256.New()
	writeFingerprintValue(fingerprint, "cloister-content-fingerprint-v2")
	projectFingerprints := make(map[string]string, len(projects))
	for projectIndex, project := range projects {
		projectFingerprint := sha256.New()
		writeFingerprintValue(fingerprint, "project", project.ID, project.Path, string(project.Kind))
		writeFingerprintValue(projectFingerprint, "cloister-project-content-fingerprint-v1", project.ID, project.Path, string(project.Kind))
		scanErr := scanProject(
			proposal,
			project,
			projectPaths[project.ID],
			nestedProjectRoots(project.ID, projectPaths),
			options,
			io.MultiWriter(fingerprint, projectFingerprint),
		)
		projectFingerprints[project.ID] = fmt.Sprintf("%x", projectFingerprint.Sum(nil))
		if scanErr != nil {
			var limitError *LimitError
			if errors.As(scanErr, &limitError) {
				if proposal != nil {
					proposal.Projects[projectIndex].IncompleteScan = true
					proposal.Projects[projectIndex].ScanIssue = limitError.Issue
					proposal.Projects[projectIndex].Recommendation = RecommendationReview
					proposal.Projects[projectIndex].Decision = DecisionReview
				}
				continue
			}
			return nil, Snapshot{}, scanErr
		}
	}
	snapshot := Snapshot{
		ContentFingerprint:  fmt.Sprintf("%x", fingerprint.Sum(nil)),
		ProjectFingerprints: projectFingerprints,
	}
	if proposal != nil {
		normalizeProposal(proposal)
		RebuildExclusions(proposal)
		if err := ValidateProposal(*proposal); err != nil {
			return nil, Snapshot{}, fmt.Errorf("validating scan proposal: %w", err)
		}
	}
	return proposal, snapshot, nil
}

func withDefaults(options Options) Options {
	if options.MaxEntriesPerProject <= 0 && options.Policy.MaxEntriesPerProject > 0 {
		options.MaxEntriesPerProject = options.Policy.MaxEntriesPerProject
	}
	if options.MaxBytesPerProject <= 0 && options.Policy.MaxBytesPerProject > 0 {
		options.MaxBytesPerProject = options.Policy.MaxBytesPerProject
	}
	if options.MaxEntriesPerProject <= 0 {
		options.MaxEntriesPerProject = DefaultMaxEntriesPerProject
	}
	if options.MaxBytesPerProject <= 0 {
		options.MaxBytesPerProject = DefaultMaxBytesPerProject
	}
	if options.LargeFileBytes <= 0 {
		options.LargeFileBytes = DefaultLargeFileBytes
	}
	if options.SourceAdapter == "" {
		options.SourceAdapter = SourceAdapterGeneric
	}
	if options.Generator == "" {
		options.Generator = "cloister"
	}
	if options.CreatedAt.IsZero() {
		options.CreatedAt = time.Now().UTC()
	}
	if options.OpenFile == nil {
		options.OpenFile = func(path string) (io.ReadCloser, error) { return os.Open(path) }
	}
	if options.Policy.Selectors == nil {
		options.Policy.Selectors = []string{}
	}
	if options.Policy.Ignore == nil {
		options.Policy.Ignore = []string{}
	}
	if options.Policy.ProjectIgnore == nil {
		options.Policy.ProjectIgnore = map[string][]string{}
	}
	if options.Policy.PrunePatterns == nil {
		options.Policy.PrunePatterns = alwaysPrunedDirectoryNames()
	}
	return options
}

func canonicalSourceRoot(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("source root is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", sanitizedError("reading source root failed", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source root must be a directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", sanitizedError("resolving source root failed", err)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", sanitizedError("resolving source root failed", err)
	}
	return absolute, nil
}

func validateProjects(
	sourceRoot string,
	descriptors []ProjectDescriptor,
	approvedProjectRoots []string,
	sourceAdapter SourceAdapter,
) ([]Project, map[string]string, error) {
	if len(descriptors) == 0 {
		return nil, nil, fmt.Errorf("at least one project descriptor is required")
	}
	approvedRoots := make([]string, 0, len(approvedProjectRoots))
	for _, root := range approvedProjectRoots {
		canonical, err := canonicalSourceRoot(root)
		if err != nil {
			return nil, nil, fmt.Errorf("validating approved project root: %w", err)
		}
		approvedRoots = append(approvedRoots, canonical)
	}
	projects := make([]Project, 0, len(descriptors))
	paths := make(map[string]string, len(descriptors))
	relativePaths := make(map[string]string, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.ID == "" {
			return nil, nil, fmt.Errorf("project ID is required")
		}
		if _, exists := paths[descriptor.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate project ID %q", descriptor.ID)
		}
		if !portableProjectPath(descriptor.Path) {
			return nil, nil, fmt.Errorf("project %q path must be a clean relative slash path", descriptor.ID)
		}
		if existing, exists := relativePaths[descriptor.Path]; exists {
			return nil, nil, fmt.Errorf("duplicate project path %q for %q and %q", descriptor.Path, existing, descriptor.ID)
		}
		if !validProjectKind(descriptor.Kind) {
			return nil, nil, fmt.Errorf("project %q has invalid kind %q", descriptor.ID, descriptor.Kind)
		}
		if descriptor.NestedRepositories < 0 {
			return nil, nil, fmt.Errorf("project %q has invalid nested repository count", descriptor.ID)
		}
		recommendation := descriptor.Recommendation
		if recommendation == "" {
			recommendation = RecommendationInclude
		}
		decision := descriptor.Decision
		if decision == "" {
			decision = DecisionInclude
		}
		reason := descriptor.Reason
		if reason == "" {
			reason = "selected project"
		}
		if !validRecommendation(recommendation) || !validDecision(decision) {
			return nil, nil, fmt.Errorf("project %q has invalid candidate decision", descriptor.ID)
		}

		expectedPath := filepath.Join(sourceRoot, filepath.FromSlash(descriptor.Path))
		path := expectedPath
		if descriptor.Root != "" {
			path = descriptor.Root
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, nil, sanitizedError(fmt.Sprintf("reading project %q failed", descriptor.ID), err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("project %q must be a real directory", descriptor.ID)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, nil, sanitizedError(fmt.Sprintf("resolving project %q failed", descriptor.ID), err)
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return nil, nil, sanitizedError(fmt.Sprintf("resolving project %q failed", descriptor.ID), err)
		}
		if containedBy(sourceRoot, resolved) {
			expectedResolved, resolveErr := filepath.EvalSymlinks(expectedPath)
			if resolveErr != nil {
				return nil, nil, sanitizedError(fmt.Sprintf("resolving project %q portable path failed", descriptor.ID), resolveErr)
			}
			expectedResolved, resolveErr = filepath.Abs(expectedResolved)
			if resolveErr != nil {
				return nil, nil, sanitizedError(fmt.Sprintf("resolving project %q portable path failed", descriptor.ID), resolveErr)
			}
			if filepath.Clean(resolved) != filepath.Clean(expectedResolved) {
				return nil, nil, fmt.Errorf("project %q root does not match its portable path", descriptor.ID)
			}
		} else if !containedByAny(approvedRoots, resolved) {
			return nil, nil, fmt.Errorf("project %q is not contained under the source root or an approved project root", descriptor.ID)
		}
		projects = append(projects, Project{
			ID: descriptor.ID, Path: descriptor.Path, Kind: descriptor.Kind,
			NestedRepositories: descriptor.NestedRepositories, Reason: reason,
			Recommendation: recommendation, Decision: decision,
		})
		paths[descriptor.ID] = resolved
		relativePaths[descriptor.Path] = descriptor.ID
	}
	resolvedPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		resolvedPaths = append(resolvedPaths, path)
	}
	sort.Strings(resolvedPaths)
	for i, parent := range resolvedPaths {
		for _, child := range resolvedPaths[i+1:] {
			if containedBy(parent, child) && sourceAdapter != SourceAdapterRepository {
				return nil, nil, fmt.Errorf("project roots are nested; select only one synchronization root")
			}
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return projects, paths, nil
}

func containedBy(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func containedByAny(roots []string, path string) bool {
	for _, root := range roots {
		if containedBy(root, path) {
			return true
		}
	}
	return false
}

func nestedProjectRoots(projectID string, projectPaths map[string]string) map[string]bool {
	root := projectPaths[projectID]
	nested := make(map[string]bool)
	for candidateID, candidateRoot := range projectPaths {
		if candidateID != projectID && containedBy(root, candidateRoot) {
			nested[candidateRoot] = true
		}
	}
	return nested
}

func scanProject(
	proposal *Proposal,
	project Project,
	root string,
	nestedRoots map[string]bool,
	options Options,
	fingerprint io.Writer,
) error {
	var entries int64
	var bytes int64
	contributors := make(map[string]subtreeContribution)
	configuredIgnore := append([]string(nil), options.Policy.Ignore...)
	configuredIgnore = append(configuredIgnore, options.Policy.ProjectIgnore[project.Path]...)
	ignorePolicy, err := brokerignore.CompileConfigured(root, configuredIgnore, nil)
	if err != nil {
		return fmt.Errorf("compiling ignore policy for project %q: %w", project.ID, err)
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			relative := safeRelativePath(root, path)
			return sanitizedError(
				fmt.Sprintf("reading metadata for project %q at %q failed", project.ID, relative),
				walkErr,
			)
		}
		if path != root && nestedRoots[path] {
			return filepath.SkipDir
		}
		relative := ""
		if path != root {
			relative, err = filepath.Rel(root, path)
			if err != nil {
				return sanitizedError(fmt.Sprintf("resolving metadata path for project %q failed", project.ID), err)
			}
			relative = filepath.ToSlash(relative)
			if ignorePolicy.Ignored(relative, entry.IsDir()) {
				if entry.IsDir() && ignorePolicy.Prunes(relative) {
					return filepath.SkipDir
				}
				return nil
			}
		}
		info, err := entry.Info()
		if err != nil {
			relative := safeRelativePath(root, path)
			return sanitizedError(
				fmt.Sprintf("reading metadata for project %q at %q failed", project.ID, relative),
				err,
			)
		}
		writeEntryFingerprint(fingerprint, relative, info, options.LargeFileBytes)
		if path == root {
			return nil
		}
		entries++
		contributorPath := strings.SplitN(relative, "/", 2)[0]
		contribution := contributors[contributorPath]
		contribution.path = contributorPath
		contribution.entries++
		if entries > options.MaxEntriesPerProject {
			contributors[contributorPath] = contribution
			return newLimitError(project.ID, LimitEntries, options.MaxEntriesPerProject, entries, contributors)
		}
		if info.Mode().IsRegular() {
			bytes += info.Size()
			contribution.bytes += info.Size()
			if bytes > options.MaxBytesPerProject {
				contributors[contributorPath] = contribution
				return newLimitError(project.ID, LimitBytes, options.MaxBytesPerProject, bytes, contributors)
			}
		}
		contributors[contributorPath] = contribution
		classified := classify(entryMetadata{
			relativePath:   relative,
			directory:      entry.IsDir(),
			size:           info.Size(),
			largeFileBytes: options.LargeFileBytes,
		})
		if proposal != nil {
			proposal.Findings = append(proposal.Findings, Finding{
				Class: classified.class, ProjectID: project.ID, Path: relative, Size: info.Size(),
				Directory: entry.IsDir(),
				Reason:    classified.reason, Recommendation: classified.recommendation, Decision: classified.decision,
			})
		}
		if entry.IsDir() && classified.prune {
			return filepath.SkipDir
		}
		if proposal != nil && info.Mode().IsRegular() && isSafeManifest(relative) &&
			(classified.class == ClassApplicationManifest || classified.class == ClassServiceManifest) {
			if err := parseSafeManifest(proposal, project.ID, path, relative, options.OpenFile); err != nil {
				return err
			}
		}
		return nil
	})
}

type subtreeContribution struct {
	path    string
	entries int64
	bytes   int64
}

func newLimitError(
	projectID string,
	kind LimitKind,
	limit int64,
	observed int64,
	contributors map[string]subtreeContribution,
) *LimitError {
	ranked := make([]subtreeContribution, 0, len(contributors))
	for _, contribution := range contributors {
		ranked = append(ranked, contribution)
	}
	sort.Slice(ranked, func(i, j int) bool {
		var left, right int64
		if kind == LimitBytes {
			left, right = ranked[i].bytes, ranked[j].bytes
		} else {
			left, right = ranked[i].entries, ranked[j].entries
		}
		if left != right {
			return left > right
		}
		return ranked[i].path < ranked[j].path
	})
	if len(ranked) > 3 {
		ranked = ranked[:3]
	}
	parts := make([]string, 0, len(ranked))
	for _, contribution := range ranked {
		if kind == LimitBytes {
			parts = append(parts, fmt.Sprintf("%s (%d bytes)", contribution.path, contribution.bytes))
		} else {
			parts = append(parts, fmt.Sprintf("%s (%d entries)", contribution.path, contribution.entries))
		}
	}
	issue := fmt.Sprintf(
		"%s scan limit of %d exceeded after observing %d; largest observed subtrees: %s",
		kind,
		limit,
		observed,
		strings.Join(parts, ", "),
	)
	return &LimitError{ProjectID: projectID, Kind: kind, Limit: limit, Observed: observed, Issue: issue}
}

func writeFingerprintValue(destination io.Writer, values ...string) {
	var length [8]byte
	for _, value := range values {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = destination.Write(length[:])
		_, _ = destination.Write([]byte(value))
	}
}

// The content fingerprint detects metadata changes that can alter a review
// decision or introduce unreviewed paths. It intentionally ignores writes that
// preserve an entry's type, permissions, and large-file classification.
func writeEntryFingerprint(destination io.Writer, relative string, info fs.FileInfo, largeFileBytes int64) {
	values := []string{
		"entry",
		relative,
		info.Mode().Type().String(),
		fmt.Sprintf("%#o", uint32(info.Mode().Perm())),
	}
	if info.Mode().IsRegular() {
		values = append(values, fmt.Sprintf("large:%t", info.Size() >= largeFileBytes))
	}
	writeFingerprintValue(destination, values...)
}

func safeRelativePath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relative) {
		return "project root"
	}
	return filepath.ToSlash(relative)
}

func classify(entry entryMetadata) classification {
	path := entry.relativePath
	directory := entry.directory
	size := entry.size
	largeFileBytes := entry.largeFileBytes
	lowerPath := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(filepath.FromSlash(path)))

	if !directory && isRepositoryAgentConfig(lowerPath, base) {
		return classification{class: ClassAgentConfig, reason: "repository-owned agent instruction", recommendation: RecommendationReview, decision: DecisionReview}
	}
	if private, prune := isHostPrivateAgentPath(lowerPath, base, directory); private {
		return excluded(ClassHostPrivateAgentState, "host-private agent state", prune)
	}
	if !directory && isCookieStore(base) {
		return classification{class: ClassSecretLocalConfig, reason: "cookie store", recommendation: RecommendationReview, decision: DecisionReview}
	}
	if isCredentialOrCertificatePath(lowerPath, base) {
		return excluded(ClassSecretLocalConfig, "credential or certificate path", directory)
	}
	if base == ".git" {
		if directory {
			return excluded(ClassDependency, "git metadata directory", true)
		}
		return excluded(ClassDependency, "git worktree metadata pointer", true)
	}
	if directory {
		if isGeneratedDotNetConfigurationPath(lowerPath) {
			return excluded(ClassGeneratedArtifact, "generated .NET configuration subtree", true)
		}
		if IsAlwaysPrunedDirectoryName(base) {
			switch base {
			case "node_modules", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache",
				".terraform", ".terragrunt-cache":
				return excluded(ClassDependency, "rebuildable dependency or cache tree", true)
			case ".direnv":
				return excluded(ClassGeneratedArtifact, "machine-local generated environment state", true)
			default:
				return excluded(ClassGeneratedArtifact, "generated artifact tree", true)
			}
		}
		return included(ClassSource, "source directory")
	}
	if isSafeConfigTemplate(base) {
		return included(ClassSource, "configuration template")
	}
	if isLocalConfigPath(lowerPath, base) {
		return classification{class: ClassSecretLocalConfig, reason: "secret or machine-local configuration path", recommendation: RecommendationReview, decision: DecisionReview}
	}
	if isDatabaseDump(lowerPath, base) {
		return excluded(ClassDatabaseDump, "database dump", false)
	}
	if strings.HasSuffix(base, ".sql") {
		return included(ClassDatabaseScript, "database source or development script")
	}
	if isDatabase(base) {
		return excluded(ClassDatabase, "database file", false)
	}
	if isApplicationManifest(base) {
		return included(ClassApplicationManifest, "application manifest")
	}
	if isServiceManifest(base) {
		return included(ClassServiceManifest, "declared service manifest")
	}
	if size >= largeFileBytes {
		return classification{class: ClassUnknownLarge, reason: "unrecognized large file", recommendation: RecommendationReview, decision: DecisionReview}
	}
	return included(ClassSource, "source or local-development input")
}

func included(class FindingClass, reason string) classification {
	return classification{class: class, reason: reason, recommendation: RecommendationInclude, decision: DecisionInclude}
}

func excluded(class FindingClass, reason string, prune bool) classification {
	return classification{class: class, reason: reason, recommendation: RecommendationExclude, decision: DecisionExclude, prune: prune}
}

func isCredentialOrCertificatePath(path, base string) bool {
	for _, segment := range splitPath(path) {
		switch segment {
		case ".ssh", ".aws", ".gnupg":
			return true
		}
	}
	switch base {
	case "credentials", "credentials.json", "credentials.yaml", "credentials.yml",
		"secrets.json", "secrets.yaml", "secrets.yml", "service-account.json",
		"application_default_credentials.json", "id_rsa", "id_ed25519", "secring.gpg":
		return true
	}
	for _, extension := range []string{".pem", ".key", ".p12", ".pfx", ".crt", ".cer"} {
		if strings.HasSuffix(base, extension) {
			return true
		}
	}
	return false
}

func isCookieStore(base string) bool {
	switch base {
	case "cookies", "cookies-journal", "safe browsing cookies",
		"safe browsing cookies-journal", "cookies.sqlite", "cookies.txt", "cookies.json":
		return true
	default:
		return filepath.Ext(base) == ".cookies"
	}
}

func isLocalConfigPath(_ string, base string) bool {
	return base == ".env" || strings.HasPrefix(base, ".env.") ||
		isDirenvConfig(base) ||
		isLocalConfigName(base) ||
		isLocalAppSettings(base) ||
		base == ".netrc" || base == ".npmrc" || base == ".pypirc"
}

// direnv configuration runs shell code and materializes environment values on
// the machine that loads it, so it is treated as machine-local rather than as
// portable source even when a repository tracks it.
func isDirenvConfig(base string) bool {
	for _, name := range direnvConfigNames() {
		if base == name || strings.HasPrefix(base, name+".") {
			return true
		}
	}
	return false
}

func direnvConfigNames() []string {
	return []string{".envrc", ".direnvrc", ".direnv"}
}

func isLocalConfigName(base string) bool {
	for _, extension := range []string{".json", ".yaml", ".yml", ".toml", ".ini", ".config"} {
		if strings.HasSuffix(base, extension) {
			stem := strings.TrimSuffix(base, extension)
			return hasFilenameSuffixToken(stem, "local")
		}
	}
	return false
}

func isLocalAppSettings(base string) bool {
	return strings.HasPrefix(base, "appsettings.") && strings.HasSuffix(base, ".json") &&
		filenameHasToken(base, "local")
}

func isSafeConfigTemplate(base string) bool {
	if !filenameHasToken(base, "example") && !filenameHasToken(base, "sample") && !filenameHasToken(base, "template") {
		return false
	}
	return strings.HasPrefix(base, ".env.") ||
		strings.HasPrefix(base, ".envrc.") ||
		strings.HasPrefix(base, ".direnvrc.") ||
		(strings.HasPrefix(base, "appsettings.") && strings.HasSuffix(base, ".json"))
}

func filenameTokens(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	})
}

func filenameHasToken(name, token string) bool {
	for _, part := range filenameTokens(name) {
		if part == token {
			return true
		}
	}
	return false
}

func hasFilenameSuffixToken(name, token string) bool {
	parts := filenameTokens(name)
	return len(parts) > 1 && parts[len(parts)-1] == token
}

func isGeneratedDotNetConfigurationPath(path string) bool {
	segments := splitPath(path)
	if len(segments) < 2 {
		return false
	}
	parent := segments[len(segments)-2]
	configuration := segments[len(segments)-1]
	return (parent == "bin" || parent == "obj") &&
		(configuration == "debug" || configuration == "release")
}

func isDatabase(base string) bool {
	return strings.HasSuffix(base, ".db") || strings.HasSuffix(base, ".sqlite") || strings.HasSuffix(base, ".sqlite3")
}

func isDatabaseDump(path, base string) bool {
	if strings.HasSuffix(base, ".dump") || strings.HasSuffix(base, ".bak") {
		return true
	}
	if !strings.HasSuffix(base, ".sql") {
		return false
	}
	stem := strings.TrimSuffix(base, ".sql")
	for _, token := range strings.FieldsFunc(stem, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	}) {
		if token == "backup" || token == "dump" {
			return true
		}
	}
	for _, segment := range splitPath(path) {
		if segment == "backups" || segment == "dumps" {
			return true
		}
	}
	return false
}

func isApplicationManifest(base string) bool {
	switch base {
	case "package.json", "go.mod", "global.json", "pyproject.toml", "cargo.toml",
		"pom.xml", "build.gradle", "build.gradle.kts", "gemfile", "requirements.txt":
		return true
	default:
		return false
	}
}

func isServiceManifest(base string) bool {
	return base == "compose.yaml" || base == "compose.yml" ||
		base == "docker"+"-compose.yaml" || base == "docker"+"-compose.yml"
}

func isRepositoryAgentConfig(path, base string) bool {
	if base == "agents.md" || base == "claude.md" || base == "instructions.md" {
		return true
	}
	segments := splitPath(path)
	rootIndex := agentStateRootIndex(segments)
	if rootIndex < 0 || rootIndex+1 >= len(segments) {
		return false
	}
	switch segments[rootIndex+1] {
	case "rules", "commands", "skills", "instructions":
		return true
	default:
		return false
	}
}

func isHostPrivateAgentPath(path, base string, directory bool) (bool, bool) {
	if base == ".mcp.json" || base == "mcp.json" {
		return true, false
	}
	segments := splitPath(path)
	rootIndex := agentStateRootIndex(segments)
	if rootIndex >= 0 && segments[rootIndex] == ".agent-grid" {
		return true, directory
	}
	if rootIndex < 0 || rootIndex+1 >= len(segments) {
		return false, false
	}
	privateDirectories := map[string]struct{}{
		"projects": {}, "agent-transcripts": {}, "transcripts": {}, "history": {},
		"session": {}, "sessions": {}, "session-env": {}, "migrations": {},
		"token": {}, "tokens": {}, "account": {}, "accounts": {}, "mcp": {},
		"inventory": {},
	}
	if _, private := privateDirectories[segments[rootIndex+1]]; private {
		return true, directory
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	extension := filepath.Ext(base)
	switch extension {
	case ".json", ".jsonl", ".yaml", ".yml", ".db", ".sqlite":
	default:
		return false, false
	}
	privateFiles := map[string]struct{}{
		"token": {}, "tokens": {}, "account": {}, "accounts": {}, "mcp": {},
		"transcript": {}, "transcripts": {}, "history": {}, "session": {},
		"session-state": {}, "migration-inventory": {}, "migration-state": {},
		"inventory": {}, "state": {},
	}
	_, private := privateFiles[stem]
	return private, false
}

func agentStateRootIndex(segments []string) int {
	for i, segment := range segments {
		switch segment {
		case ".cursor", ".claude", ".agent", ".agent-grid":
			return i
		}
	}
	return -1
}

func splitPath(path string) []string {
	return strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' })
}

func isSafeManifest(path string) bool {
	base := strings.ToLower(filepath.Base(filepath.FromSlash(path)))
	return base == "package.json" || base == "go.mod" || base == "global.json" || isServiceManifest(base)
}

func parseSafeManifest(proposal *Proposal, projectID, absolutePath, relativePath string, open OpenFileFunc) error {
	file, err := open(absolutePath)
	if err != nil {
		return sanitizedError(
			fmt.Sprintf("opening safe manifest %q in project %q failed", relativePath, projectID),
			err,
		)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return sanitizedError(
			fmt.Sprintf("reading safe manifest %q in project %q failed", relativePath, projectID),
			err,
		)
	}
	if len(data) > maxManifestBytes {
		return fmt.Errorf("safe manifest %q in project %q exceeds metadata parsing limit", relativePath, projectID)
	}

	base := strings.ToLower(filepath.Base(filepath.FromSlash(relativePath)))
	switch base {
	case "package.json":
		return parsePackageManifest(proposal, projectID, relativePath, data)
	case "go.mod":
		return parseGoManifest(proposal, projectID, relativePath, data)
	case "global.json":
		return parseGlobalManifest(proposal, projectID, relativePath, data)
	default:
		return parseComposeManifest(proposal, projectID, relativePath, data)
	}
}

func parsePackageManifest(proposal *Proposal, projectID, path string, data []byte) error {
	var manifest struct {
		Engines map[string]string          `json:"engines"`
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parsing application manifest %q in project %q failed", path, projectID)
	}
	if version, ok := manifest.Engines["node"]; ok {
		proposal.Runtimes = append(proposal.Runtimes, Runtime{ProjectID: projectID, Name: "node", Version: version, EvidencePath: path})
	}
	for name := range manifest.Scripts {
		proposal.Commands = append(proposal.Commands, Command{ProjectID: projectID, Name: name, Path: path})
	}
	return nil
}

func parseGoManifest(proposal *Proposal, projectID, path string, data []byte) error {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "go" {
			proposal.Runtimes = append(proposal.Runtimes, Runtime{ProjectID: projectID, Name: "go", Version: fields[1], EvidencePath: path})
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("parsing application manifest %q in project %q failed", path, projectID)
	}
	return nil
}

func parseGlobalManifest(proposal *Proposal, projectID, path string, data []byte) error {
	var manifest struct {
		SDK struct {
			Version string `json:"version"`
		} `json:"sdk"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parsing application manifest %q in project %q failed", path, projectID)
	}
	if manifest.SDK.Version != "" {
		proposal.Runtimes = append(proposal.Runtimes, Runtime{ProjectID: projectID, Name: "dotnet", Version: manifest.SDK.Version, EvidencePath: path})
	}
	return nil
}

func parseComposeManifest(proposal *Proposal, projectID, path string, data []byte) error {
	names, err := composeServiceNames(data)
	if err != nil {
		return fmt.Errorf("parsing service manifest %q in project %q failed", path, projectID)
	}
	for _, name := range names {
		proposal.Services = append(proposal.Services, Service{ProjectID: projectID, Name: name, Path: path})
	}
	return nil
}

func alwaysPrunedDirectoryNames() []string {
	return append(
		AlwaysPrunedDirectoryNames(),
		"bin/debug", "bin/release", "obj/debug", "obj/release",
	)
}
