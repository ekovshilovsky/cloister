// Proprietary and confidential. All rights reserved.

// Package broker manages project-scoped synchronized copies inside Cloister
// virtual machines. It deliberately exposes lifecycle operations, not files.
package broker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"cloister.io/internal/vm"
)

// SyncBroker is the engine-neutral lifecycle for one project session.
type SyncBroker interface {
	Create(context.Context, SessionSpec) error
	Flush(context.Context, SessionSpec) error
	Pause(context.Context, SessionSpec) error
	Resume(context.Context, SessionSpec) error
	Terminate(context.Context, SessionSpec) error
	Status(context.Context, SessionSpec) (Status, error)
}

// SessionSpec identifies one profile and project pair. HostRoot is private
// local state; names and guest paths use only sanitized names and opaque IDs.
type SessionSpec struct {
	Profile            string
	ProjectID          string
	Name               string
	HostRoot           string
	GuestRoot          string
	SSH                vm.SSHAccess
	Ignore             []string
	MaxEntries         uint64
	MaxStagingFileSize string
}

// BuildSessionSpec canonicalizes one project and derives stable identifiers.
func BuildSessionSpec(profile, hostRoot string, access vm.SSHAccess, extraIgnore []string) (SessionSpec, error) {
	original, err := filepath.Abs(hostRoot)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("making broker project root absolute: %w", err)
	}
	info, err := os.Lstat(original)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("reading broker project root %q: %w", hostRoot, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return SessionSpec{}, fmt.Errorf("broker project root %q is a symlink; select the real project directory", hostRoot)
	}
	if !info.IsDir() {
		return SessionSpec{}, fmt.Errorf("broker project root %q is not a directory", hostRoot)
	}
	canonical, err := filepath.EvalSymlinks(hostRoot)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("resolving broker project root %q: %w", hostRoot, err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("making broker project root absolute: %w", err)
	}
	idBytes := sha256.Sum256([]byte("cloister-project-v1\x00" + canonical))
	projectID := fmt.Sprintf("%x", idBytes[:12])
	base := sanitize(filepath.Base(canonical))
	profileID := sanitize(profile)
	return SessionSpec{
		Profile:            profile,
		ProjectID:          projectID,
		Name:               "cloister-" + profileID + "-" + projectID,
		HostRoot:           canonical,
		GuestRoot:          "~/workspaces/" + base + "-" + projectID[:12],
		SSH:                access,
		Ignore:             append([]string(nil), extraIgnore...),
		MaxEntries:         250_000,
		MaxStagingFileSize: "2 GiB",
	}, nil
}

func sanitize(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	lastDash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			result.WriteRune(r)
			lastDash = false
		} else if !lastDash && result.Len() > 0 {
			result.WriteByte('-')
			lastDash = true
		}
	}
	clean := strings.Trim(result.String(), "-")
	if clean == "" {
		return "project"
	}
	return clean
}

// State is the conservative high-level state reported by a broker.
type State string

const (
	StateMissing State = "missing"
	StateActive  State = "active"
	StatePaused  State = "paused"
	StateProblem State = "problem"
)

// Status captures teardown-relevant health. A clean barrier requires no
// conflict and no endpoint problem.
type Status struct {
	State         State
	ConflictCount int
	Problems      []string
	Description   string
}

// Clean validates that a completed flush can be treated as durable.
func (s Status) Clean() error {
	if s.State == StateMissing {
		return fmt.Errorf("synchronization session is missing")
	}
	if s.State != StateActive && s.State != StatePaused && s.State != StateProblem {
		return fmt.Errorf("synchronization session has unknown state %q", s.State)
	}
	if s.ConflictCount > 0 {
		return fmt.Errorf("synchronization has %d unresolved conflict(s); resolve them before clean teardown", s.ConflictCount)
	}
	if len(s.Problems) > 0 || s.State == StateProblem {
		return fmt.Errorf("synchronization has unresolved endpoint problems: %s", strings.Join(s.Problems, "; "))
	}
	return nil
}

// GuestRootCommand returns a shell fragment for the generated safe guest path.
// When requireEmpty is true, an existing non-empty target is rejected because
// a new three-way history must not merge two independently populated roots.
func GuestRootCommand(spec SessionSpec, requireEmpty bool) (string, error) {
	if !strings.HasPrefix(spec.GuestRoot, "~/workspaces/") {
		return "", fmt.Errorf("unsafe broker guest root %q", spec.GuestRoot)
	}
	relative := strings.TrimPrefix(spec.GuestRoot, "~/")
	for _, r := range relative {
		if !(unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("/-_.", r)) {
			return "", fmt.Errorf("unsafe broker guest root %q", spec.GuestRoot)
		}
	}
	command := `target="$HOME/` + relative + `"; mkdir -p -- "$target" && test -d "$target" && test ! -L "$target"`
	if requireEmpty {
		command += ` && test -z "$(find "$target" -mindepth 1 -maxdepth 1 -print -quit)"`
	}
	return command, nil
}

// GuestShellCommand launches the guest's login shell in the synchronized copy.
func GuestShellCommand(spec SessionSpec) (string, error) {
	if _, err := GuestRootCommand(spec, false); err != nil {
		return "", err
	}
	relative := strings.TrimPrefix(spec.GuestRoot, "~/")
	return `cd "$HOME/` + relative + `" && exec "${SHELL:-/bin/bash}" -l`, nil
}

// GuestCommand runs an existing guest shell command from the synchronized copy.
func GuestCommand(spec SessionSpec, command string) (string, error) {
	if _, err := GuestRootCommand(spec, false); err != nil {
		return "", err
	}
	relative := strings.TrimPrefix(spec.GuestRoot, "~/")
	return `cd "$HOME/` + relative + `" && ` + command, nil
}
