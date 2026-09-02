// Package workspace discovers bounded multi-project workspace sessions.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/vm"
)

const (
	DefaultMaxEntryCount      = 200_000
	DefaultMaxStagingFileSize = "2 GiB"
)

var defaultSelectors = []string{"apps/*", "tools/*"}

var minimalMandatoryIgnore = []string{".git", "node_modules/"}

// SelectedProject is one project selected by workspace root and selector
// validation.
type SelectedProject struct {
	Path     string
	Relative string
}

// SelectProjects exposes the same root resolution and selector validation used
// by workspace session discovery for reusable metadata adapters.
func SelectProjects(startDir, home string, cfg config.WorkspaceConfig) (string, []SelectedProject, error) {
	root, selected, err := selectProjects(startDir, home, cfg)
	if err != nil {
		return "", nil, err
	}
	projects := make([]SelectedProject, 0, len(selected))
	for path, relative := range selected {
		projects = append(projects, SelectedProject{Path: path, Relative: relative})
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Relative != projects[j].Relative {
			return projects[i].Relative < projects[j].Relative
		}
		return projects[i].Path < projects[j].Path
	})
	return root, projects, nil
}

// Discover resolves the profile routing root and creates one stable broker
// session specification for every selected project directory.
func Discover(profile, startDir, home string, cfg config.WorkspaceConfig, access vm.SSHAccess) ([]broker.SessionSpec, error) {
	root, projects, err := selectProjects(startDir, home, cfg)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(projects))
	for path := range projects {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	specs := make([]broker.SessionSpec, 0, len(paths))
	for _, path := range paths {
		spec, err := BuildProjectSpec(profile, root, path, cfg, access)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	if err := broker.ValidateSessionSpecs(specs); err != nil {
		return nil, fmt.Errorf("validating workspace guest paths: %w", err)
	}
	return specs, nil
}

// ProjectSession returns the session specification a workspace-mode profile
// uses for exactly one of its projects. The project must live under the
// configured routing root and be selected by the configured selectors, so an
// explicitly requested project carries the same policy collection activation
// would give it.
func ProjectSession(profile, projectPath, startDir, home string, cfg config.WorkspaceConfig, access vm.SSHAccess) (broker.SessionSpec, error) {
	root, projects, err := selectProjects(startDir, home, cfg)
	if err != nil {
		return broker.SessionSpec{}, err
	}
	canonical, err := canonicalProjectDirectory(projectPath)
	if err != nil {
		return broker.SessionSpec{}, err
	}
	relative, err := filepath.Rel(root, canonical)
	if err != nil || escapes(relative) {
		return broker.SessionSpec{}, fmt.Errorf(
			"project %q is outside the workspace root %q",
			workspaceRelativeDiagnostic(relative),
			".",
		)
	}
	if _, ok := projects[canonical]; !ok {
		return broker.SessionSpec{}, fmt.Errorf(
			"project %q is not selected by the workspace selectors below %q",
			filepath.ToSlash(relative),
			".",
		)
	}
	return BuildProjectSpec(profile, root, canonical, cfg, access)
}

// BuildProjectSpec creates the broker session specification for one already
// validated workspace project directory below root. It is the single place
// where workspace project policy (per-project ignores, minimal mandatory
// ignores, and the synchronization guardrails) is applied.
func BuildProjectSpec(profile, root, projectPath string, cfg config.WorkspaceConfig, access vm.SSHAccess) (broker.SessionSpec, error) {
	relative, err := filepath.Rel(root, projectPath)
	if err != nil || escapes(relative) {
		return broker.SessionSpec{}, fmt.Errorf(
			"workspace project %q is outside the workspace root %q",
			workspaceRelativeDiagnostic(relative),
			".",
		)
	}
	project := filepath.ToSlash(relative)
	if project == "." {
		if len(cfg.Selectors) != 1 || cfg.Selectors[0] != "." {
			return broker.SessionSpec{}, fmt.Errorf(`workspace project "." requires the sole selector "."`)
		}
		if err := validateSourceRootSelector(root, cfg.Selectors); err != nil {
			return broker.SessionSpec{}, err
		}
	}

	extra := append([]string(nil), cfg.Ignore...)
	extra = append(extra, cfg.ProjectIgnore[project]...)
	spec, err := broker.BuildSessionSpec(profile, projectPath, access, extra)
	if err != nil {
		return broker.SessionSpec{}, fmt.Errorf("building workspace session for %q: %w", project, err)
	}
	// A workspace collection mirrors each project below the routing root. The
	// sole root selector has no relative path to mirror, so its one project uses
	// the root directory's readable base name instead.
	guestPath := project
	if guestPath == "." {
		guestPath = filepath.Base(root)
	}
	guestRoot, err := broker.WorkspaceGuestRoot(guestPath)
	if err != nil {
		return broker.SessionSpec{}, fmt.Errorf("building workspace session for %q: %w", project, err)
	}
	spec.GuestRoot = guestRoot
	spec.MandatoryIgnore = append([]string(nil), minimalMandatoryIgnore...)
	spec.MaxEntries = cfg.MaxEntryCount
	if spec.MaxEntries == 0 {
		spec.MaxEntries = DefaultMaxEntryCount
	}
	spec.MaxStagingFileSize = cfg.MaxStagingFileSize
	if spec.MaxStagingFileSize == "" {
		spec.MaxStagingFileSize = DefaultMaxStagingFileSize
	}
	spec.ProbeMode = "assume"
	spec.SkipGitignores = true
	return spec, nil
}

// selectProjects resolves the routing root and returns every selected project
// keyed by its canonical directory with its root-relative path as the value.
func selectProjects(startDir, home string, cfg config.WorkspaceConfig) (string, map[string]string, error) {
	rootValue := cfg.Root
	if rootValue == "" {
		rootValue = startDir
	}
	root, err := config.ResolveWorkspaceDir(rootValue, home)
	if err != nil {
		return "", nil, fmt.Errorf("resolving workspace root: %w", err)
	}
	root, err = canonicalProjectDirectory(root)
	if err != nil {
		return "", nil, err
	}

	selectors := append([]string(nil), cfg.Selectors...)
	if len(selectors) == 0 {
		selectors = append([]string(nil), defaultSelectors...)
	}
	if err := validateSourceRootSelector(root, selectors); err != nil {
		return "", nil, err
	}

	byCanonical := make(map[string]string)
	for _, selector := range selectors {
		if err := validateSelector(selector); err != nil {
			return "", nil, err
		}
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(selector)))
		if err != nil {
			return "", nil, fmt.Errorf("invalid workspace selector %q: %w", selector, err)
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				return "", nil, fmt.Errorf("reading workspace match %q: %w", match, err)
			}
			if !info.IsDir() {
				continue
			}
			canonical, err := canonicalProjectDirectory(match)
			if err != nil {
				return "", nil, err
			}
			relative, err := filepath.Rel(root, canonical)
			if err != nil || escapes(relative) {
				return "", nil, fmt.Errorf("workspace selector %q resolved outside its root", selector)
			}
			byCanonical[canonical] = filepath.ToSlash(relative)
		}
	}
	if len(byCanonical) == 0 {
		return "", nil, fmt.Errorf("workspace selectors matched no project directories below %q", root)
	}

	paths := make([]string, 0, len(byCanonical))
	for path := range byCanonical {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for i, parent := range paths {
		for _, child := range paths[i+1:] {
			relative, err := filepath.Rel(parent, child)
			if err == nil && relative != "." && !escapes(relative) {
				return "", nil, fmt.Errorf("workspace projects %q and %q are nested; select only one synchronization root", byCanonical[parent], byCanonical[child])
			}
		}
	}

	selected := make(map[string]bool, len(byCanonical))
	for _, relative := range byCanonical {
		selected[relative] = true
	}
	for project := range cfg.ProjectIgnore {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(project)))
		if project == "" || clean != project || escapes(filepath.FromSlash(clean)) || !selected[project] {
			return "", nil, fmt.Errorf("workspace project_ignore key %q does not name a selected project", project)
		}
	}
	return root, byCanonical, nil
}

func validateSelector(selector string) error {
	if selector == "" || filepath.IsAbs(selector) {
		return fmt.Errorf("workspace selector %q must be a non-empty relative glob", selector)
	}
	clean := filepath.Clean(filepath.FromSlash(selector))
	if (selector != "." && clean == ".") || escapes(clean) || strings.ContainsRune(selector, '\x00') {
		return fmt.Errorf("workspace selector %q escapes or selects the routing root", selector)
	}
	return nil
}

func validateSourceRootSelector(root string, selectors []string) error {
	hasRootSelector := false
	for _, selector := range selectors {
		if selector == "." {
			hasRootSelector = true
		}
	}
	if !hasRootSelector {
		return nil
	}
	if len(selectors) != 1 {
		return fmt.Errorf(`workspace selector "." must be used alone; keep either the root or its children, not both`)
	}
	gitMetadata := filepath.Join(root, ".git")
	info, err := os.Lstat(gitMetadata)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(`workspace selector "." requires "." to be a repository root`)
	}
	if err != nil {
		return fmt.Errorf(`checking repository metadata at "." failed`)
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return fmt.Errorf(`workspace selector "." requires "." to be a repository root`)
	}
	return nil
}

func workspaceRelativeDiagnostic(relative string) string {
	if relative == "" {
		return "requested project"
	}
	return filepath.ToSlash(relative)
}

func canonicalProjectDirectory(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("reading workspace directory %q: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("workspace directory %q must be a real directory", path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolving workspace directory %q: %w", path, err)
	}
	return filepath.Abs(canonical)
}

func escapes(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative)
}
