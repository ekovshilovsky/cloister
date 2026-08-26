// Package source provides reusable workspace project catalog adapters.
package source

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cloister.io/internal/config"
	"cloister.io/internal/workspace"
	"cloister.io/internal/workspace/scan"
)

const (
	currentManifestFormatVersion = 1
	maxMetadataBytes             = 1 << 20
)

// Source resolves a bounded project catalog without reading project contents.
type Source interface {
	Load() (Result, error)
}

// Result is portable project and policy metadata plus validated host roots.
type Result struct {
	Root                 string
	Adapter              scan.SourceAdapter
	Projects             []scan.ProjectDescriptor
	Policy               scan.Policy
	ApprovedProjectRoots []string
	MetadataDigest       string
}

// ScanOptions returns the adapter-controlled portion of scanner options.
func (result Result) ScanOptions() scan.Options {
	return scan.Options{
		SourceRoot:           result.Root,
		Projects:             append([]scan.ProjectDescriptor(nil), result.Projects...),
		SourceAdapter:        result.Adapter,
		Policy:               clonePolicy(result.Policy),
		ApprovedProjectRoots: append([]string(nil), result.ApprovedProjectRoots...),
	}
}

// GenericSelector adapts the existing selector-based workspace configuration.
type GenericSelector struct {
	StartDir string
	Home     string
	Config   config.WorkspaceConfig
}

// Load resolves generic projects through workspace's canonical selector path.
func (source GenericSelector) Load() (Result, error) {
	root, selected, err := workspace.SelectProjects(source.StartDir, source.Home, source.Config)
	if err != nil {
		return Result{}, err
	}
	projects := make([]scan.ProjectDescriptor, 0, len(selected))
	for _, project := range selected {
		projects = append(projects, scan.ProjectDescriptor{
			ID: project.Relative, Path: project.Relative, Kind: scan.ProjectShared,
		})
	}
	selectors := append([]string(nil), source.Config.Selectors...)
	if len(selectors) == 0 {
		selectors = []string{"apps/*", "tools/*"}
	}
	maxEntries := int64(source.Config.MaxEntryCount)
	if maxEntries == 0 {
		maxEntries = scan.DefaultMaxEntriesPerProject
	}
	return Result{
		Root:     root,
		Adapter:  scan.SourceAdapterGeneric,
		Projects: projects,
		Policy: scan.Policy{
			Selectors:            selectors,
			Ignore:               append([]string(nil), source.Config.Ignore...),
			ProjectIgnore:        cloneStringSliceMap(source.Config.ProjectIgnore),
			MaxStagingFileSize:   source.Config.MaxStagingFileSize,
			MaxEntriesPerProject: maxEntries,
			MaxBytesPerProject:   scan.DefaultMaxBytesPerProject,
		},
	}, nil
}

// LookupEnvFunc resolves environment overrides without reading environment
// files.
type LookupEnvFunc func(name string) (string, bool)

// ManifestOptions configures the manifest-managed project adapter.
type ManifestOptions struct {
	Root                     string
	OpenFile                 scan.OpenFileFunc
	LookupEnv                LookupEnvFunc
	ApprovedExternalRoots    []string
	WorktreeSets             []string
	ProjectsDirEnv           string
	WorktreesDirEnv          string
	WorkspaceRootEnv         string
	WorkspaceProjectsSuffix  string
	WorkspaceWorktreesSuffix string
}

// Manifest is a generic manifest-managed project source.
type Manifest struct {
	options ManifestOptions
}

// NewManifest constructs a manifest source. Load performs all validation.
func NewManifest(options ManifestOptions) Manifest {
	return Manifest{options: options}
}

type manifestDocument struct {
	FormatVersion int                `json:"formatVersion"`
	ProjectsDir   string             `json:"projectsDir"`
	WorktreesDir  string             `json:"worktreesDir"`
	Projects      []manifestProject  `json:"projects"`
	WorktreeSets  []manifestWorktree `json:"worktreeSets"`
	Policy        manifestPolicy     `json:"policy"`
}

type localDocument struct {
	FormatVersion int               `json:"formatVersion"`
	ProjectsDir   string            `json:"projectsDir"`
	WorktreesDir  string            `json:"worktreesDir"`
	Projects      []manifestProject `json:"projects"`
}

type manifestProject struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Stack   string `json:"stack"`
	HubName string `json:"hubName"`
}

type manifestWorktree struct {
	ID       string   `json:"id"`
	Path     string   `json:"path"`
	Projects []string `json:"projects"`
}

type manifestPolicy struct {
	Selectors            []string            `json:"selectors"`
	Ignore               []string            `json:"ignore"`
	ProjectIgnore        map[string][]string `json:"projectIgnore"`
	MaxStagingFileSize   string              `json:"maxStagingFileSize"`
	MaxEntriesPerProject int64               `json:"maxEntriesPerProject"`
	MaxBytesPerProject   int64               `json:"maxBytesPerProject"`
}

// Load reads only the canonical and optional local metadata files.
func (source Manifest) Load() (Result, error) {
	options := source.options
	if options.Root == "" {
		return Result{}, fmt.Errorf("manifest source root is required")
	}
	root, err := canonicalDirectory(options.Root)
	if err != nil {
		return Result{}, fmt.Errorf("validating manifest source root: %w", err)
	}
	open := options.OpenFile
	if open == nil {
		open = func(path string) (io.ReadCloser, error) { return os.Open(path) }
	}
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	projectsDirEnv := valueOrDefault(options.ProjectsDirEnv, "WORKSPACE_PROJECTS_DIR")
	worktreesDirEnv := valueOrDefault(options.WorktreesDirEnv, "WORKSPACE_WORKTREES_DIR")
	workspaceRootEnv := valueOrDefault(options.WorkspaceRootEnv, "WORKSPACE_ROOT")
	workspaceProjectsSuffix := valueOrDefault(options.WorkspaceProjectsSuffix, "projects")
	workspaceWorktreesSuffix := valueOrDefault(options.WorkspaceWorktreesSuffix, "worktrees")

	manifestPath := filepath.Join(root, "manifest", "projects.json")
	localPath := filepath.Join(root, ".workspace.local.json")
	if err := requireRealMetadataDirectory(filepath.Dir(manifestPath), "manifest metadata directory"); err != nil {
		return Result{}, err
	}
	if _, err := requireRegularMetadataFile(manifestPath, false, "manifest metadata"); err != nil {
		return Result{}, err
	}
	if _, err := requireRegularMetadataFile(localPath, true, "local workspace metadata"); err != nil {
		return Result{}, err
	}

	var manifest manifestDocument
	manifestData, err := readJSON(open, manifestPath, &manifest, false)
	if err != nil {
		return Result{}, fmt.Errorf("reading manifest metadata: %w", err)
	}
	if err := validateFormatVersion(manifest.FormatVersion); err != nil {
		return Result{}, err
	}
	if manifest.Projects == nil {
		return Result{}, fmt.Errorf("manifest projects collection is required")
	}

	var local localDocument
	localPresent, localData, err := readOptionalJSON(open, localPath, &local)
	if err != nil {
		return Result{}, fmt.Errorf("reading local workspace metadata: %w", err)
	}
	if localPresent {
		if err := validateFormatVersion(local.FormatVersion); err != nil {
			return Result{}, err
		}
	}

	projects, err := overlayProjects(manifest.Projects, local.Projects)
	if err != nil {
		return Result{}, err
	}
	approvedRoots, err := canonicalApprovedRoots(options.ApprovedExternalRoots)
	if err != nil {
		return Result{}, err
	}
	projectsRoot, projectsPortableRoot, err := resolveCatalogRoot(
		root, manifest.ProjectsDir, local.ProjectsDir, "projects",
		projectsDirEnv, workspaceRootEnv, workspaceProjectsSuffix, lookup, approvedRoots,
	)
	if err != nil {
		return Result{}, fmt.Errorf("resolving projects root: %w", err)
	}
	descriptors := make([]scan.ProjectDescriptor, 0, len(projects))
	projectByName := make(map[string]manifestProject, len(projects))
	localNames := make(map[string]bool, len(local.Projects))
	for _, project := range local.Projects {
		localNames[project.Name] = true
	}
	for _, project := range projects {
		projectByName[project.Name] = project
		kind := scan.ProjectShared
		if localNames[project.Name] {
			kind = scan.ProjectLocal
		}
		descriptor, descriptorErr := projectDescriptor(
			project.Name, project.Path, kind, root, projectsRoot, projectsPortableRoot, approvedRoots,
		)
		if descriptorErr != nil {
			return Result{}, descriptorErr
		}
		descriptors = append(descriptors, descriptor)
	}

	if err := validateWorktreeCatalog(manifest.WorktreeSets, projectByName); err != nil {
		return Result{}, err
	}
	requestedSets := make(map[string]bool, len(options.WorktreeSets))
	for _, id := range options.WorktreeSets {
		if id == "" || requestedSets[id] {
			return Result{}, fmt.Errorf("worktree set IDs must be non-empty and unique")
		}
		requestedSets[id] = true
	}
	var worktreesRoot, worktreesPortableRoot string
	if len(requestedSets) > 0 {
		worktreesRoot, worktreesPortableRoot, err = resolveCatalogRoot(
			root, manifest.WorktreesDir, local.WorktreesDir, "worktrees",
			worktreesDirEnv, workspaceRootEnv, workspaceWorktreesSuffix, lookup, approvedRoots,
		)
		if err != nil {
			return Result{}, fmt.Errorf("resolving worktrees root: %w", err)
		}
	}
	for _, set := range manifest.WorktreeSets {
		if !requestedSets[set.ID] {
			continue
		}
		delete(requestedSets, set.ID)
		for _, projectName := range set.Projects {
			relative := filepath.ToSlash(filepath.Join(set.Path, projectName))
			descriptor, descriptorErr := projectDescriptor(
				"worktree:"+set.ID+":"+projectName,
				relative,
				scan.ProjectWorktree,
				root,
				worktreesRoot,
				worktreesPortableRoot,
				approvedRoots,
			)
			if descriptorErr != nil {
				return Result{}, descriptorErr
			}
			descriptors = append(descriptors, descriptor)
		}
	}
	legacySetIDs := make([]string, 0, len(requestedSets))
	for id := range requestedSets {
		legacySetIDs = append(legacySetIDs, id)
	}
	sort.Strings(legacySetIDs)
	for _, setID := range legacySetIDs {
		if !portableID(setID) || !portablePath(setID) {
			return Result{}, fmt.Errorf("requested worktree set has an invalid ID")
		}
		matched := false
		for _, project := range projects {
			relative := filepath.ToSlash(filepath.Join(setID, project.Name))
			candidate := filepath.Join(worktreesRoot, filepath.FromSlash(relative))
			if _, statErr := os.Lstat(candidate); errors.Is(statErr, fs.ErrNotExist) {
				continue
			} else if statErr != nil {
				return Result{}, fmt.Errorf("reading requested worktree set failed")
			}
			descriptor, descriptorErr := projectDescriptor(
				"worktree:"+setID+":"+project.Name,
				relative,
				scan.ProjectWorktree,
				root,
				worktreesRoot,
				worktreesPortableRoot,
				approvedRoots,
			)
			if descriptorErr != nil {
				return Result{}, descriptorErr
			}
			descriptors = append(descriptors, descriptor)
			matched = true
		}
		if !matched {
			return Result{}, fmt.Errorf("requested worktree set contains no canonical projects")
		}
		delete(requestedSets, setID)
	}
	if err := validateDescriptors(root, descriptors); err != nil {
		return Result{}, err
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].ID < descriptors[j].ID })

	policy, err := buildManifestPolicy(manifest.Policy, descriptors, projectsPortableRoot)
	if err != nil {
		return Result{}, err
	}
	metadataDigest := digestMetadata(manifestData, localData, localPresent)
	return Result{
		Root:                 root,
		Adapter:              scan.SourceAdapterWorkspaceManifest,
		Projects:             descriptors,
		Policy:               policy,
		ApprovedProjectRoots: approvedRoots,
		MetadataDigest:       metadataDigest,
	}, nil
}

func requireRealMetadataDirectory(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		var pathError *fs.PathError
		if errors.As(err, &pathError) {
			err = pathError.Err
		}
		return fmt.Errorf("checking %s failed: %w", label, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory containing a non-symlink regular file", label)
	}
	return nil
}

func requireRegularMetadataFile(path string, optional bool, label string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if optional && errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		var pathError *fs.PathError
		if errors.As(err, &pathError) {
			err = pathError.Err
		}
		return false, fmt.Errorf("checking %s failed: %w", label, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%s must be a non-symlink regular file", label)
	}
	return true, nil
}

func digestMetadata(manifestData, localData []byte, localPresent bool) string {
	hash := sha256.New()
	values := []struct {
		name string
		data []byte
	}{{"manifest/projects.json", manifestData}}
	if localPresent {
		values = append(values, struct {
			name string
			data []byte
		}{".workspace.local.json", localData})
	}
	for _, value := range values {
		hash.Write([]byte(value.name))
		hash.Write([]byte{0})
		hash.Write(value.data)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func readJSON(open scan.OpenFileFunc, path string, target any, optional bool) ([]byte, error) {
	file, err := open(path)
	if err != nil {
		if optional && errors.Is(err, fs.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMetadataBytes {
		return nil, fmt.Errorf("metadata exceeds parsing limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, fmt.Errorf("invalid JSON metadata")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("invalid JSON metadata")
	}
	return data, nil
}

func readOptionalJSON(open scan.OpenFileFunc, path string, target any) (bool, []byte, error) {
	data, err := readJSON(open, path, target, true)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil, nil
	}
	return err == nil, data, err
}

func validateFormatVersion(version int) error {
	if version != 0 && version != currentManifestFormatVersion {
		return fmt.Errorf("unsupported manifest format version %d", version)
	}
	return nil
}

func overlayProjects(canonical, local []manifestProject) ([]manifestProject, error) {
	byName := make(map[string]manifestProject, len(canonical)+len(local))
	for _, project := range canonical {
		if !portableID(project.Name) || !portablePath(project.Path) {
			return nil, fmt.Errorf("manifest project has an invalid name or path")
		}
		if _, exists := byName[project.Name]; exists {
			return nil, fmt.Errorf("duplicate project ID %q", project.Name)
		}
		byName[project.Name] = project
	}
	localNames := make(map[string]bool, len(local))
	for _, project := range local {
		if !portableID(project.Name) || (project.Path != "" && !portablePath(project.Path)) {
			return nil, fmt.Errorf("local project has an invalid name or path")
		}
		if localNames[project.Name] {
			return nil, fmt.Errorf("duplicate local project %q", project.Name)
		}
		localNames[project.Name] = true
		existing, exists := byName[project.Name]
		if exists {
			project = mergeProject(existing, project)
		}
		if project.Path == "" {
			return nil, fmt.Errorf("local project %q has no path", project.Name)
		}
		byName[project.Name] = project
	}
	projects := make([]manifestProject, 0, len(byName))
	for _, project := range byName {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, nil
}

func mergeProject(canonical, local manifestProject) manifestProject {
	merged := canonical
	if local.Name != "" {
		merged.Name = local.Name
	}
	if local.Path != "" {
		merged.Path = local.Path
	}
	if local.Repo != "" {
		merged.Repo = local.Repo
	}
	if local.Branch != "" {
		merged.Branch = local.Branch
	}
	if local.Stack != "" {
		merged.Stack = local.Stack
	}
	if local.HubName != "" {
		merged.HubName = local.HubName
	}
	return merged
}

func validateWorktreeCatalog(sets []manifestWorktree, projects map[string]manifestProject) error {
	ids := make(map[string]bool, len(sets))
	for _, set := range sets {
		if !portableID(set.ID) || !portablePath(set.Path) {
			return fmt.Errorf("worktree set has an invalid ID or path")
		}
		if ids[set.ID] {
			return fmt.Errorf("duplicate worktree set ID %q", set.ID)
		}
		ids[set.ID] = true
		projectNames := make(map[string]bool, len(set.Projects))
		for _, projectName := range set.Projects {
			if _, ok := projects[projectName]; !ok {
				return fmt.Errorf("worktree set %q references unknown project %q", set.ID, projectName)
			}
			if projectNames[projectName] {
				return fmt.Errorf("worktree set %q contains duplicate project %q", set.ID, projectName)
			}
			projectNames[projectName] = true
		}
	}
	return nil
}

func canonicalApprovedRoots(roots []string) ([]string, error) {
	result := make([]string, 0, len(roots))
	for _, root := range roots {
		canonical, err := canonicalDirectory(root)
		if err != nil {
			return nil, fmt.Errorf("invalid approved external root: %w", err)
		}
		result = append(result, canonical)
	}
	sort.Strings(result)
	return result, nil
}

func resolveCatalogRoot(
	sourceRoot, manifestValue, localValue, defaultValue, directEnv, workspaceEnv string,
	workspaceSuffix string,
	lookup LookupEnvFunc,
	approvedRoots []string,
) (string, string, error) {
	configured := manifestValue
	if configured == "" {
		configured = defaultValue
	}
	value := configured
	if localValue != "" {
		value = localValue
	}
	if !portablePath(workspaceSuffix) {
		return "", "", fmt.Errorf("workspace catalog suffix is invalid")
	}
	if workspaceRoot, ok := lookup(workspaceEnv); ok && workspaceRoot != "" {
		value = filepath.Join(workspaceRoot, filepath.FromSlash(workspaceSuffix))
	}
	if direct, ok := lookup(directEnv); ok && direct != "" {
		value = direct
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(sourceRoot, filepath.FromSlash(value))
	}
	canonical, err := canonicalDirectory(value)
	if err != nil {
		return "", "", fmt.Errorf("catalog root is unavailable")
	}
	if !containedBy(sourceRoot, canonical) && !containedByAny(approvedRoots, canonical) {
		return "", "", fmt.Errorf("external catalog root is not within an approved external root")
	}
	portableRoot := defaultValue
	if containedBy(sourceRoot, canonical) {
		relative, relErr := filepath.Rel(sourceRoot, canonical)
		if relErr != nil || !portablePath(filepath.ToSlash(relative)) {
			return "", "", fmt.Errorf("catalog root has no safe portable path")
		}
		portableRoot = filepath.ToSlash(relative)
	}
	return canonical, portableRoot, nil
}

func valueOrDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func projectDescriptor(
	id, relative string,
	kind scan.ProjectKind,
	sourceRoot, catalogRoot, portableRoot string,
	approvedRoots []string,
) (scan.ProjectDescriptor, error) {
	if !portableID(id) || !portablePath(relative) {
		return scan.ProjectDescriptor{}, fmt.Errorf("project %q has an invalid path", id)
	}
	path := filepath.Join(catalogRoot, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil {
		return scan.ProjectDescriptor{}, fmt.Errorf("reading project %q failed", id)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return scan.ProjectDescriptor{}, fmt.Errorf("project %q must be a real directory", id)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return scan.ProjectDescriptor{}, fmt.Errorf("resolving project %q failed", id)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return scan.ProjectDescriptor{}, fmt.Errorf("resolving project %q failed", id)
	}
	if !containedBy(sourceRoot, resolved) && !containedByAny(approvedRoots, resolved) {
		return scan.ProjectDescriptor{}, fmt.Errorf("project %q is outside approved roots", id)
	}
	portable := filepath.ToSlash(filepath.Join(filepath.FromSlash(portableRoot), filepath.FromSlash(relative)))
	descriptor := scan.ProjectDescriptor{ID: id, Path: portable, Kind: kind}
	expected := filepath.Join(sourceRoot, filepath.FromSlash(portable))
	expected, _ = filepath.Abs(expected)
	if filepath.Clean(expected) != filepath.Clean(resolved) {
		descriptor.Root = resolved
	}
	return descriptor, nil
}

func validateDescriptors(sourceRoot string, descriptors []scan.ProjectDescriptor) error {
	ids := make(map[string]bool, len(descriptors))
	paths := make(map[string]bool, len(descriptors))
	physicalRoots := make(map[string]bool, len(descriptors))
	roots := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if ids[descriptor.ID] {
			return fmt.Errorf("duplicate project ID %q", descriptor.ID)
		}
		if paths[descriptor.Path] {
			return fmt.Errorf("duplicate project path %q", descriptor.Path)
		}
		ids[descriptor.ID] = true
		paths[descriptor.Path] = true
		root := descriptor.Root
		if root == "" {
			root = filepath.Join(sourceRoot, filepath.FromSlash(descriptor.Path))
		}
		if physicalRoots[root] {
			return fmt.Errorf("duplicate physical project root")
		}
		physicalRoots[root] = true
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for i, parent := range roots {
		for _, child := range roots[i+1:] {
			relative, err := filepath.Rel(parent, child)
			if err == nil && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != ".." {
				return fmt.Errorf("project roots are nested")
			}
		}
	}
	return nil
}

func buildManifestPolicy(value manifestPolicy, descriptors []scan.ProjectDescriptor, projectsPortableRoot string) (scan.Policy, error) {
	maxEntries := value.MaxEntriesPerProject
	if maxEntries <= 0 {
		maxEntries = scan.DefaultMaxEntriesPerProject
	}
	maxBytes := value.MaxBytesPerProject
	if maxBytes <= 0 {
		maxBytes = scan.DefaultMaxBytesPerProject
	}
	selected := make(map[string]string, len(descriptors))
	for _, descriptor := range descriptors {
		selected[descriptor.Path] = descriptor.Path
		prefix := projectsPortableRoot + "/"
		if strings.HasPrefix(descriptor.Path, prefix) {
			selected[strings.TrimPrefix(descriptor.Path, prefix)] = descriptor.Path
		}
	}
	projectIgnore := make(map[string][]string, len(value.ProjectIgnore))
	for key, patterns := range value.ProjectIgnore {
		path, ok := selected[key]
		if !ok || !portablePath(key) || patterns == nil {
			return scan.Policy{}, fmt.Errorf("projectIgnore key %q does not name a selected project", key)
		}
		projectIgnore[path] = append([]string(nil), patterns...)
	}
	return scan.Policy{
		Selectors:            append([]string(nil), value.Selectors...),
		Ignore:               append([]string(nil), value.Ignore...),
		ProjectIgnore:        projectIgnore,
		MaxStagingFileSize:   value.MaxStagingFileSize,
		MaxEntriesPerProject: maxEntries,
		MaxBytesPerProject:   maxBytes,
	}, nil
}

func canonicalDirectory(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

func containedByAny(roots []string, path string) bool {
	for _, root := range roots {
		if containedBy(root, path) {
			return true
		}
	}
	return false
}

func containedBy(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func portableID(value string) bool {
	return value != "" && !strings.ContainsAny(value, `/\`+"\x00") && value != "." && value != ".."
}

func portablePath(value string) bool {
	if value == "" || filepath.IsAbs(value) || strings.ContainsAny(value, `\`+"\x00") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	return clean == value && value != "." && value != ".." && !strings.HasPrefix(value, "../")
}

func clonePolicy(policy scan.Policy) scan.Policy {
	policy.Selectors = append([]string(nil), policy.Selectors...)
	policy.Ignore = append([]string(nil), policy.Ignore...)
	policy.ProjectIgnore = cloneStringSliceMap(policy.ProjectIgnore)
	policy.PrunePatterns = append([]string(nil), policy.PrunePatterns...)
	return policy
}

func cloneStringSliceMap(values map[string][]string) map[string][]string {
	result := make(map[string][]string, len(values))
	for key, entries := range values {
		result[key] = append([]string(nil), entries...)
	}
	return result
}
