// Package vcsbroker maps synchronized guest paths to host repositories and
// executes a constrained set of VCS commands on the host.
package vcsbroker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cloister.io/internal/broker"
)

// Mapping identifies one registered project and a relative working directory.
type Mapping struct {
	Spec     broker.SessionSpec
	Relative string
}

// Mapper owns the explicit guest-to-host workspace registry.
type Mapper struct {
	guestHome string
	projects  []mappedProject
}

type mappedProject struct {
	spec      broker.SessionSpec
	guestRoot string
}

// NewMapper expands stable guest roots against the actual guest home.
func NewMapper(guestHome string, specs []broker.SessionSpec) (*Mapper, error) {
	if guestHome == "" || !filepath.IsAbs(guestHome) {
		return nil, fmt.Errorf("guest home must be an absolute path")
	}
	mapper := &Mapper{guestHome: filepath.Clean(guestHome)}
	for _, spec := range specs {
		if !strings.HasPrefix(spec.GuestRoot, "~/workspaces/") {
			return nil, fmt.Errorf("unsafe registered guest root %q", spec.GuestRoot)
		}
		root := filepath.Join(mapper.guestHome, strings.TrimPrefix(spec.GuestRoot, "~/"))
		mapper.projects = append(mapper.projects, mappedProject{spec: spec, guestRoot: filepath.Clean(root)})
	}
	sort.Slice(mapper.projects, func(i, j int) bool {
		return len(mapper.projects[i].guestRoot) > len(mapper.projects[j].guestRoot)
	})
	return mapper, nil
}

// MapGuest performs lexical containment mapping. ResolveHost must be called
// after the pre-command flush to reject host symlink escapes.
func (m *Mapper) MapGuest(guestCWD string) (Mapping, error) {
	if m == nil || !filepath.IsAbs(guestCWD) {
		return Mapping{}, fmt.Errorf("guest working directory %q is not absolute", guestCWD)
	}
	guestCWD = filepath.Clean(guestCWD)
	for _, project := range m.projects {
		relative, err := filepath.Rel(project.guestRoot, guestCWD)
		if err != nil || escapes(relative) {
			continue
		}
		return Mapping{Spec: project.spec, Relative: relative}, nil
	}
	return Mapping{}, fmt.Errorf("guest working directory %q is outside every registered workspace project", guestCWD)
}

// ResolveHost maps a registered relative path and verifies its real host path
// stays inside the canonical project root.
func (m *Mapper) ResolveHost(mapping Mapping) (string, error) {
	return m.resolveHost(mapping, true)
}

// ResolveHostPath maps a file or directory argument and rejects symlink
// escapes from the registered host project.
func (m *Mapper) ResolveHostPath(mapping Mapping) (string, error) {
	return m.resolveHost(mapping, false)
}

func (m *Mapper) resolveHost(mapping Mapping, requireDirectory bool) (string, error) {
	// Validate containment before the value touches the filesystem. The relative
	// component is already constrained by MapGuest, but re-reject any escaping
	// component locally, then confirm the lexical join stays within the project,
	// so a guest-controlled path can never reach EvalSymlinks/Stat unchecked.
	if escapes(mapping.Relative) {
		return "", fmt.Errorf("mapped relative path %q escapes project %q", mapping.Relative, mapping.Spec.HostRoot)
	}
	target := filepath.Join(mapping.Spec.HostRoot, mapping.Relative)
	if rel, err := filepath.Rel(mapping.Spec.HostRoot, target); err != nil || escapes(rel) {
		return "", fmt.Errorf("mapped host path %q escapes project %q", target, mapping.Spec.HostRoot)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolving mapped host working directory %q: %w", target, err)
	}
	// Re-check containment on the symlink-resolved real path before using it.
	// The prefix guard against the non-tainted, canonical HostRoot both rejects
	// symlink escapes and acts as the sanitizer barrier for the filesystem sink
	// below, so a guest-controlled path can never reach os.Stat outside the
	// project.
	root := filepath.Clean(mapping.Spec.HostRoot)
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("mapped host working directory %q escapes project %q", resolved, root)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if requireDirectory && !info.IsDir() {
		return "", fmt.Errorf("mapped host working directory %q is not a directory", resolved)
	}
	return resolved, nil
}

func escapes(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative)
}
