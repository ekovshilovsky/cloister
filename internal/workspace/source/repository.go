package source

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cloister.io/internal/config"
	"cloister.io/internal/workspace/layout"
	"cloister.io/internal/workspace/scan"
)

const (
	// DefaultMaxRepositoryWalkDepth bounds traversal while allowing deeply
	// grouped multi-repository workspaces.
	DefaultMaxRepositoryWalkDepth = 64
	// DefaultMaxRepositoryRoots prevents an unexpected tree from producing an
	// unbounded proposal while remaining generous for large workspace roots.
	DefaultMaxRepositoryRoots = 10_000
	// DefaultMaxRepositoryDirectories bounds broad trees whose repositories may
	// appear only at leaves. Directories are the walk's only unit of work, so
	// this bound also caps directory fan-out at any width or depth.
	DefaultMaxRepositoryDirectories = 100_000
	// repositoryDirectoryBatch bounds how many entries one read materializes so
	// a single very wide directory never enters memory whole.
	repositoryDirectoryBatch = 256
)

// Boundary discovery does not need build output conventions. The metadata
// scanner keeps bin and obj traversable because they can contain source.
var repositoryWalkOnlyPrunedDirectories = map[string]struct{}{
	"bin": {}, "obj": {},
}

type RepositoryOptions struct {
	Root            string
	Config          config.WorkspaceConfig
	MaxDepth        int
	MaxDirectories  int
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
	maxDirectories := options.MaxDirectories
	if maxDirectories <= 0 {
		maxDirectories = DefaultMaxRepositoryDirectories
	}
	if err := validateRootSelector(root, options.Config.Selectors); err != nil {
		return Result{}, err
	}

	roots, err := discoverRepositoryRoots(root, maxDepth, maxDirectories, maxRepositories)
	if err != nil {
		return Result{}, err
	}
	descriptors := repositoryDescriptors(root, roots)
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

// discoverRepositoryRoots reads directory entries only to find subdirectories
// to descend into, so plain files are skipped without a stat and never consume
// a bound. Fan-out is bounded by the directories the walk actually visits.
func discoverRepositoryRoots(
	root string,
	maxDepth int,
	maxDirectories int,
	maxRepositories int,
) ([]repositoryRoot, error) {
	var repositories []repositoryRoot
	var directories int
	var walk func(string, string, int) error
	walk = func(directory, relative string, depth int) error {
		directories++
		if directories > maxDirectories {
			return fmt.Errorf(
				"repository directories visited bound of %d exceeded at %q",
				maxDirectories,
				relative,
			)
		}
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

		handle, err := os.Open(directory)
		if err != nil {
			return repositoryWalkError(fmt.Sprintf("reading repository directories at %q failed", relative), err)
		}
		defer handle.Close()
		for {
			entries, readErr := handle.ReadDir(repositoryDirectoryBatch)
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				name := entry.Name()
				if scan.IsAlwaysPrunedDirectoryName(name) {
					continue
				}
				if _, prune := repositoryWalkOnlyPrunedDirectories[name]; prune {
					continue
				}
				childRelative := childRelativePath(relative, name)
				child := filepath.Join(directory, name)
				info, err := os.Lstat(child)
				if err != nil {
					return repositoryWalkError(fmt.Sprintf("checking repository directory at %q failed", childRelative), err)
				}
				if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					continue
				}
				childDepth := depth + 1
				if childDepth > maxDepth {
					return fmt.Errorf(
						"repository walk depth bound of %d exceeded at %q",
						maxDepth,
						childRelative,
					)
				}
				if err := walk(child, childRelative, childDepth); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return repositoryWalkError(fmt.Sprintf("reading repository directories at %q failed", relative), readErr)
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

func childRelativePath(parent, name string) string {
	if parent == "." {
		return name
	}
	return parent + "/" + name
}

func repositoryDescriptors(sourceRoot string, repositories []repositoryRoot) []scan.ProjectDescriptor {
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
		hostRoot := sourceRoot
		if repository.path != "." {
			hostRoot = filepath.Join(sourceRoot, filepath.FromSlash(repository.path))
		}
		descriptors = append(descriptors, scan.ProjectDescriptor{
			ID: repository.path, Path: repository.path, Kind: repository.kind,
			NestedRepositories: nested, Reason: reason,
			Recommendation: recommendation, Decision: decision,
			Org: layout.OriginOrg(hostRoot),
		})
	}
	return descriptors
}

func repositoryContains(parent, child string) bool {
	if parent == "." {
		return child != "."
	}
	return strings.HasPrefix(child, parent+"/")
}

func canonicalRepositoryRoot(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("repository source root is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", repositoryWalkError("resolving repository source root failed", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", repositoryWalkError("resolving repository source root failed", err)
	}
	canonical := filepath.Join(resolvedParent, filepath.Base(absolute))
	info, err := os.Lstat(canonical)
	if err != nil {
		return "", repositoryWalkError("reading repository source root failed", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("repository source root must be a real directory")
	}
	return canonical, nil
}

type sanitizedRepositoryWalkError struct {
	message string
	cause   error
}

func (err *sanitizedRepositoryWalkError) Error() string {
	return err.message
}

func (err *sanitizedRepositoryWalkError) Unwrap() error {
	return err.cause
}

func repositoryWalkError(message string, cause error) error {
	var pathError *fs.PathError
	if errors.As(cause, &pathError) {
		cause = pathError.Err
	}
	return &sanitizedRepositoryWalkError{message: message, cause: cause}
}

func validateRootSelector(root string, selectors []string) error {
	hasRootSelector := false
	for _, selector := range selectors {
		if !portableRepositorySelector(selector) {
			return fmt.Errorf("configured workspace selector must be a portable relative glob")
		}
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
	info, err := os.Lstat(filepath.Join(root, ".git"))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf(`workspace selector "." requires "." to be a repository root`)
	}
	if err != nil {
		return repositoryWalkError(`checking repository metadata at "." failed`, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return fmt.Errorf(`workspace selector "." requires "." to be a repository root`)
	}
	return nil
}

func portableRepositorySelector(selector string) bool {
	if selector == "" || filepath.IsAbs(selector) || strings.ContainsAny(selector, `\`+"\x00") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(selector)))
	if clean != selector || selector == ".." || strings.HasPrefix(selector, "../") {
		return false
	}
	_, err := filepath.Match(filepath.FromSlash(selector), "portable-check")
	return err == nil
}
