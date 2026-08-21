// Proprietary and confidential. All rights reserved.

// Package workspace resolves host project paths to authorized Cloister profiles.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cloister.io/internal/config"
)

// Resolution is the deterministic profile route for one canonical host path.
type Resolution struct {
	Profile string
	Path    string
	Scope   string
}

type candidate struct {
	name  string
	scope string
}

// ResolveProfile canonicalizes requested and selects the profile whose
// configured start_dir is the most specific directory containing it. Equal
// scopes are rejected as ambiguous rather than depending on map iteration.
func ResolveProfile(requested, homeDir string, profiles map[string]*config.Profile) (Resolution, error) {
	canonical, err := canonicalDirectory(requested)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolving project path: %w", err)
	}

	all := make([]candidate, 0, len(profiles))
	matches := make([]candidate, 0, len(profiles))
	for name, profile := range profiles {
		configured := config.DefaultStartDir
		if profile != nil && profile.StartDir != "" {
			configured = profile.StartDir
		}
		displayScope, resolveErr := config.ResolveWorkspaceDir(configured, homeDir)
		if resolveErr != nil {
			displayScope = configured
		}
		all = append(all, candidate{name: name, scope: displayScope})

		if profile == nil || resolveErr != nil {
			continue
		}
		scope, scopeErr := canonicalDirectory(displayScope)
		if scopeErr != nil || !contains(scope, canonical) {
			continue
		}
		matches = append(matches, candidate{name: name, scope: scope})
	}

	if len(matches) == 0 {
		return Resolution{}, fmt.Errorf("project path %q is not within any configured profile start_dir; candidate profiles: %s", canonical, formatCandidates(all))
	}

	sort.Slice(matches, func(i, j int) bool {
		if len(matches[i].scope) != len(matches[j].scope) {
			return len(matches[i].scope) > len(matches[j].scope)
		}
		return matches[i].name < matches[j].name
	})
	mostSpecific := len(matches[0].scope)
	var tied []candidate
	for _, match := range matches {
		if len(match.scope) != mostSpecific {
			break
		}
		tied = append(tied, match)
	}
	if len(tied) > 1 {
		return Resolution{}, fmt.Errorf("project path %q matches equally specific profiles: %s; make one start_dir more specific", canonical, formatCandidates(tied))
	}

	return Resolution{Profile: matches[0].name, Path: canonical, Scope: matches[0].scope}, nil
}

func canonicalDirectory(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("making %q absolute: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("reading %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalizing %q: %w", path, err)
	}
	return filepath.Clean(canonical), nil
}

func contains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func formatCandidates(candidates []candidate) string {
	if len(candidates) == 0 {
		return "none configured"
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name < candidates[j].name })
	formatted := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		formatted = append(formatted, fmt.Sprintf("%s (%s)", candidate.name, candidate.scope))
	}
	return strings.Join(formatted, ", ")
}
