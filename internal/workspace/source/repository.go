package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cloister.io/internal/config"
	"cloister.io/internal/workspace/scan"
)

const (
	// DefaultMaxRepositoryWalkDepth bounds traversal while allowing deeply
	// grouped multi-repository workspaces.
	DefaultMaxRepositoryWalkDepth = 64
	// DefaultMaxRepositoryRoots prevents an unexpected tree from producing an
	// unbounded proposal while remaining generous for large workspace roots.
	DefaultMaxRepositoryRoots = 10_000
)

var repositoryPrunedDirectories = map[string]struct{}{
	".aws": {}, ".direnv": {}, ".git": {}, ".gnupg": {}, ".mypy_cache": {},
	".next": {}, ".pytest_cache": {}, ".ssh": {}, ".venv": {}, "__pycache__": {},
	"bin": {}, "coverage": {}, "dist": {}, "node_modules": {}, "obj": {}, "venv": {},
}

type RepositoryOptions struct {
	Root            string
	Config          config.WorkspaceConfig
	MaxDepth        int
	MaxRepositories int
}

type RepositoryCatalog struct {
	options RepositoryOptions
}

func NewRepositoryCatalog(options RepositoryOptions) RepositoryCatalog {
	return RepositoryCatalog{options: options}
}

func (source RepositoryCatalog) Load() (Result, error) {
	options := source.options
	root, err := canonicalRepositoryRoot(options.Root)
	if err != nil {
		return Result{}, err
	}
	maxDepth := options.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxRepositoryWalkDepth
	}
	maxRepositories := options.MaxRepositories
	if maxRepositories <= 0 {
		maxRepositories = DefaultMaxRepositoryRoots
	}

	roots, err := discoverRepositoryRoots(root, maxDepth, maxRepositories)
	if err != nil {
		return Result{}, err
	}
	descriptors, err := repositoryDescriptors(roots, options.Config.Selectors)
	if err != nil {
		return Result{}, err
	}
	if len(descriptors) == 0 {
		return Result{}, fmt.Errorf("repository discovery found no repositories below the source root")
	}

	maxEntries := int64(options.Config.MaxEntryCount)
	if maxEntries <= 0 {
		maxEntries = scan.DefaultMaxEntriesPerProject
	}
	projectIgnore := make(map[string][]string)
	discovered := make(map[string]bool, len(descriptors))
	for _, descriptor := range descriptors {
		discovered[descriptor.Path] = true
	}
	for project, patterns := range options.Config.ProjectIgnore {
		if discovered[project] {
			projectIgnore[project] = append([]string(nil), patterns...)
		}
	}
	return Result{
		Root:     root,
		Adapter:  scan.SourceAdapterRepository,
		Projects: descriptors,
		Policy: scan.Policy{
			Selectors:            append([]string(nil), options.Config.Selectors...),
			Ignore:               append([]string(nil), options.Config.Ignore...),
			ProjectIgnore:        projectIgnore,
			MaxStagingFileSize:   options.Config.MaxStagingFileSize,
			MaxEntriesPerProject: maxEntries,
			MaxBytesPerProject:   scan.DefaultMaxBytesPerProject,
		},
	}, nil
}

type repositoryRoot struct {
	path string
	kind scan.ProjectKind
}

func discoverRepositoryRoots(root string, maxDepth, maxRepositories int) ([]repositoryRoot, error) {
	var repositories []repositoryRoot
	var walk func(string, string, int) error
	walk = func(directory, relative string, depth int) error {
		gitMetadata := filepath.Join(directory, ".git")
		info, err := os.Lstat(gitMetadata)
		switch {
		case err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0:
			repositories = append(repositories, repositoryRoot{path: relative, kind: scan.ProjectRepository})
		case err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0:
			repositories = append(repositories, repositoryRoot{path: relative, kind: scan.ProjectWorktree})
		case err == nil:
		case errors.Is(err, fs.ErrNotExist):
		default:
			return repositoryWalkError(fmt.Sprintf("checking repository metadata at %q failed", relative), err)
		}
		if len(repositories) > maxRepositories {
			return fmt.Errorf(
				"repository count bound of %d exceeded at %q",
				maxRepositories,
				relative,
			)
		}

		entries, err := os.ReadDir(directory)
		if err != nil {
			return repositoryWalkError(fmt.Sprintf("reading repository directories at %q failed", relative), err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if _, prune := repositoryPrunedDirectories[name]; prune {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				continue
			}
			childDepth := depth + 1
			childRelative := name
			if relative != "." {
				childRelative = relative + "/" + name
			}
			if childDepth > maxDepth {
				return fmt.Errorf(
					"repository walk depth bound of %d exceeded at %q",
					maxDepth,
					childRelative,
				)
			}
			if err := walk(filepath.Join(directory, name), childRelative, childDepth); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, ".", 0); err != nil {
		return nil, err
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].path < repositories[j].path })
	return repositories, nil
}

func repositoryDescriptors(repositories []repositoryRoot, selectors []string) ([]scan.ProjectDescriptor, error) {
	descriptors := make([]scan.ProjectDescriptor, 0, len(repositories))
	for index, repository := range repositories {
		nested := 0
		for candidateIndex, candidate := range repositories {
			if index != candidateIndex && repositoryContains(repository.path, candidate.path) {
				nested++
			}
		}
		recommendation := scan.RecommendationInclude
		decision := scan.DecisionInclude
		reason := "canonical repository"
		if repository.kind == scan.ProjectWorktree {
			reason = "git worktree checkout"
		}
		if nested > 0 {
			recommendation = scan.RecommendationReview
			decision = scan.DecisionReview
			reason = fmt.Sprintf("contains %d nested repositories; synchronizing it would overlap them", nested)
			if nested == 1 {
				reason = "contains 1 nested repository; synchronizing it would overlap it"
			}
		}
		selected, err := repositorySelected(repository.path, selectors)
		if err != nil {
			return nil, err
		}
		if selected && nested == 0 {
			decision = scan.DecisionInclude
		}
		descriptors = append(descriptors, scan.ProjectDescriptor{
			ID: repository.path, Path: repository.path, Kind: repository.kind,
			NestedRepositories: nested, Reason: reason,
			Recommendation: recommendation, Decision: decision,
		})
	}
	return descriptors, nil
}

func repositoryContains(parent, child string) bool {
	if parent == "." {
		return child != "."
	}
	return strings.HasPrefix(child, parent+"/")
}

func repositorySelected(path string, selectors []string) (bool, error) {
	for _, selector := range selectors {
		matched, err := filepath.Match(filepath.FromSlash(selector), filepath.FromSlash(path))
		if err != nil {
			return false, fmt.Errorf("configured workspace selector is invalid")
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func canonicalRepositoryRoot(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("repository source root is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", repositoryWalkError("reading repository source root failed", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("repository source root must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", repositoryWalkError("resolving repository source root failed", err)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", repositoryWalkError("resolving repository source root failed", err)
	}
	return absolute, nil
}

func repositoryWalkError(message string, cause error) error {
	var pathError *fs.PathError
	if errors.As(cause, &pathError) {
		cause = pathError.Err
	}
	return fmt.Errorf("%s: %w", message, cause)
}
