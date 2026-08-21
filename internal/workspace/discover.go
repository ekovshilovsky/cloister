// Proprietary and confidential. All rights reserved.

// Package workspace discovers bounded multi-project workspace sessions.
package workspace

import (
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

// Discover resolves the profile routing root and creates one stable broker
// session specification for every selected project directory.
func Discover(profile, startDir, home string, cfg config.WorkspaceConfig, access vm.SSHAccess) ([]broker.SessionSpec, error) {
	rootValue := cfg.Root
	if rootValue == "" {
		rootValue = startDir
	}
	root, err := config.ResolveWorkspaceDir(rootValue, home)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace root: %w", err)
	}
	root, err = canonicalProjectDirectory(root)
	if err != nil {
		return nil, err
	}

	selectors := append([]string(nil), cfg.Selectors...)
	if len(selectors) == 0 {
		selectors = append([]string(nil), defaultSelectors...)
	}

	byCanonical := make(map[string]string)
	for _, selector := range selectors {
		if err := validateSelector(selector); err != nil {
			return nil, err
		}
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(selector)))
		if err != nil {
			return nil, fmt.Errorf("invalid workspace selector %q: %w", selector, err)
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				return nil, fmt.Errorf("reading workspace match %q: %w", match, err)
			}
			if !info.IsDir() {
				continue
			}
			canonical, err := canonicalProjectDirectory(match)
			if err != nil {
				return nil, err
			}
			relative, err := filepath.Rel(root, canonical)
			if err != nil || relative == "." || escapes(relative) {
				return nil, fmt.Errorf("workspace selector %q resolved outside its root", selector)
			}
			byCanonical[canonical] = filepath.ToSlash(relative)
		}
	}
	if len(byCanonical) == 0 {
		return nil, fmt.Errorf("workspace selectors matched no project directories below %q", root)
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
				return nil, fmt.Errorf("workspace projects %q and %q are nested; select only one synchronization root", byCanonical[parent], byCanonical[child])
			}
		}
	}

	usedExtra := make(map[string]bool)
	specs := make([]broker.SessionSpec, 0, len(paths))
	for _, path := range paths {
		relative := byCanonical[path]
		extra := append([]string(nil), cfg.Ignore...)
		if projectExtra, ok := cfg.ProjectIgnore[relative]; ok {
			extra = append(extra, projectExtra...)
			usedExtra[relative] = true
		}
		if relative == "tools/rockauto-scraper" {
			extra = append(extra, "data/raw/")
		}
		spec, err := broker.BuildSessionSpec(profile, path, access, extra)
		if err != nil {
			return nil, fmt.Errorf("building workspace session for %q: %w", relative, err)
		}
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
		specs = append(specs, spec)
	}
	for project := range cfg.ProjectIgnore {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(project)))
		if project == "" || clean != project || escapes(filepath.FromSlash(clean)) || !usedExtra[project] {
			return nil, fmt.Errorf("workspace project_ignore key %q does not name a selected project", project)
		}
	}
	return specs, nil
}

func validateSelector(selector string) error {
	if selector == "" || filepath.IsAbs(selector) {
		return fmt.Errorf("workspace selector %q must be a non-empty relative glob", selector)
	}
	clean := filepath.Clean(filepath.FromSlash(selector))
	if clean == "." || escapes(clean) || strings.ContainsRune(selector, '\x00') {
		return fmt.Errorf("workspace selector %q escapes or selects the routing root", selector)
	}
	return nil
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
