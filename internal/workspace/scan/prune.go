package scan

import (
	"sort"
	"strings"
)

var alwaysPrunedDirectoryNameSet = map[string]struct{}{
	".agent-grid": {}, ".aws": {}, ".direnv": {}, ".git": {}, ".gnupg": {},
	".mypy_cache": {}, ".next": {}, ".playwright-data": {}, ".pytest_cache": {},
	".ssh": {}, ".terraform": {}, ".terragrunt-cache": {}, ".turbo": {},
	".venv": {}, "__pycache__": {}, "coverage": {}, "dist": {},
	"node_modules": {}, "venv": {},
}

// AlwaysPrunedDirectoryNames returns rebuildable, generated, repository
// metadata, and private directory names that every workspace walk must prune.
func AlwaysPrunedDirectoryNames() []string {
	names := make([]string, 0, len(alwaysPrunedDirectoryNameSet))
	for name := range alwaysPrunedDirectoryNameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsAlwaysPrunedDirectoryName reports whether every workspace walk must avoid
// descending into a directory with this exact name.
func IsAlwaysPrunedDirectoryName(name string) bool {
	_, pruned := alwaysPrunedDirectoryNameSet[name]
	return pruned || strings.HasPrefix(name, ".playwright-data-")
}
