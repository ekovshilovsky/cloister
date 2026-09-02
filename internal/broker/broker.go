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

	brokerignore "cloister.io/internal/broker/ignore"
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

// ProfileReconciler is an optional capability for brokers that can safely
// remove obsolete sessions from one complete profile workspace collection.
type ProfileReconciler interface {
	ReconcileProfile(context.Context, string, []SessionSpec) error
}

// GuestRootVerifier refuses destructive preparation when a live session in
// the same VM profile already synchronizes to the requested guest root.
type GuestRootVerifier interface {
	VerifyGuestRootAvailable(context.Context, SessionSpec, string) error
}

// SessionSpec identifies one profile and project pair. HostRoot is private
// local state and never reaches the guest. Session names use a sanitized
// profile name and an opaque project ID. Guest paths are readable: standalone
// sessions use the project and parent names plus the complete project ID,
// while workspace collections use the sanitized project path relative to the
// workspace root.
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
	ProbeMode          string
	SkipGitignores     bool
	// MandatoryIgnore overrides the legacy broker mandatory policy when it is
	// non-nil. Workspace collections use a deliberately minimal policy.
	MandatoryIgnore []string
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
	profileID := sanitize(profile)
	return SessionSpec{
		Profile:            profile,
		ProjectID:          projectID,
		Name:               "cloister-" + profileID + "-" + projectID,
		HostRoot:           canonical,
		GuestRoot:          standaloneGuestRoot(canonical, projectID),
		SSH:                access,
		Ignore:             append([]string(nil), extraIgnore...),
		MaxEntries:         250_000,
		MaxStagingFileSize: "2 GiB",
		ProbeMode:          "assume",
	}, nil
}

// standaloneGuestRoot keeps the project and parent names visible while the
// complete project identity makes the mapping injective for distinct project
// IDs. The delimiter cannot occur in a project ID, so no suffix can be parsed
// as part of the readable prefix.
func standaloneGuestRoot(canonical, projectID string) string {
	base := sanitize(filepath.Base(canonical))
	parentPath := filepath.Dir(canonical)
	readable := base
	if filepath.Dir(parentPath) == parentPath {
		return "~/workspaces/" + readable + "--" + projectID
	}
	parent := sanitize(filepath.Base(parentPath))
	readable += "-" + parent
	return "~/workspaces/" + readable + "--" + projectID
}

// ValidateSessionSpecs enforces that one activation set cannot assign a guest
// root or project identity to multiple host projects. Callers that construct
// specs through different discovery paths share this validation at the broker
// lifecycle boundary.
func ValidateSessionSpecs(specs []SessionSpec) error {
	guestRoots := make(map[string]SessionSpec, len(specs))
	projectIDs := make(map[string]SessionSpec, len(specs))
	for _, spec := range specs {
		if _, err := guestRootRelative(spec); err != nil {
			return err
		}
		if _, err := projectIdentity(spec); err != nil {
			return err
		}
		if prior, ok := guestRoots[spec.GuestRoot]; ok {
			return fmt.Errorf("host projects %q and %q both claim guest path %q", prior.HostRoot, spec.HostRoot, spec.GuestRoot)
		}
		if prior, ok := projectIDs[spec.ProjectID]; ok && prior.HostRoot != spec.HostRoot {
			return fmt.Errorf("host projects %q and %q share project identity %q", prior.HostRoot, spec.HostRoot, spec.ProjectID)
		}
		guestRoots[spec.GuestRoot] = spec
		projectIDs[spec.ProjectID] = spec
	}
	return nil
}

// CompilePolicy returns the deterministic ignore policy for a session.
func CompilePolicy(spec SessionSpec) (brokerignore.Policy, error) {
	if spec.SkipGitignores {
		return brokerignore.CompileConfigured(spec.HostRoot, spec.Ignore, spec.MandatoryIgnore)
	}
	if spec.MandatoryIgnore != nil {
		return brokerignore.CompileWithMandatory(spec.HostRoot, spec.Ignore, spec.MandatoryIgnore)
	}
	return brokerignore.Compile(spec.HostRoot, spec.Ignore)
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

// workspaceSegment sanitizes one path segment for use inside a guest workspace
// path. Unlike sanitize, letter case is preserved so a guest directory reads
// like the host project it mirrors, and any character outside the guest-root
// character set collapses to a single dash.
func workspaceSegment(value string) (string, error) {
	var result strings.Builder
	lastDash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' || r == '.' {
			result.WriteRune(r)
			lastDash = false
		} else if !lastDash && result.Len() > 0 {
			result.WriteByte('-')
			lastDash = true
		}
	}
	clean := strings.Trim(result.String(), "-")
	if clean == "" || clean == "." || clean == ".." {
		return "", fmt.Errorf("workspace path segment %q has no usable guest name", value)
	}
	return clean, nil
}

// WorkspaceGuestRoot maps a project path expressed relative to the workspace
// root onto the guest path that mirrors it under ~/workspaces.
//
// Mirroring the workspace layout is what keeps a large collection navigable.
// Naming a guest directory after the project base name alone collapses every
// sibling that shares that name -- a repository checked out once per worktree
// set, for example -- into one name per project plus a disambiguating hash,
// which leaves the reader no way to tell the copies apart. The relative path
// already distinguishes them, so reusing it removes the need for the hash.
func WorkspaceGuestRoot(relative string) (string, error) {
	segments := strings.Split(filepath.ToSlash(relative), "/")
	cleaned := make([]string, 0, len(segments))
	for _, segment := range segments {
		safe, err := workspaceSegment(segment)
		if err != nil {
			return "", err
		}
		cleaned = append(cleaned, safe)
	}
	return "~/workspaces/" + strings.Join(cleaned, "/"), nil
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

	// GuestRoot is the guest path the live session actually synchronizes to,
	// which is not necessarily the one the current specification asks for: a
	// session's endpoints are fixed when it is created, so a specification
	// whose guest path has since changed must be recreated rather than
	// resumed. Empty when no live session was reported.
	GuestRoot string

	// HostRoot is the host path the live session actually synchronizes from.
	// It is compared with the canonical requested path before an existing
	// session is adopted. Empty when no live session was reported.
	HostRoot string
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

// guestRootRelative validates the managed guest root and returns its path
// relative to $HOME. Only paths under ~/workspaces/ with a restricted character
// set are accepted so the generated shell fragments cannot touch anything else.
func guestRootRelative(spec SessionSpec) (string, error) {
	if !strings.HasPrefix(spec.GuestRoot, "~/workspaces/") {
		return "", fmt.Errorf("unsafe broker guest root %q", spec.GuestRoot)
	}
	relative := strings.TrimPrefix(spec.GuestRoot, "~/")
	for _, r := range relative {
		if !(unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("/-_.", r)) {
			return "", fmt.Errorf("unsafe broker guest root %q", spec.GuestRoot)
		}
	}
	// The character set above admits both "." and "/", so a dot-only segment
	// would satisfy it while walking back out of ~/workspaces. That matters
	// because GuestRootResetCommand interpolates this path into an rm -rf.
	for _, segment := range strings.Split(relative, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("unsafe broker guest root %q", spec.GuestRoot)
		}
	}
	return relative, nil
}

func projectIdentity(spec SessionSpec) (string, error) {
	if len(spec.ProjectID) != 24 {
		return "", fmt.Errorf("invalid broker project ID %q", spec.ProjectID)
	}
	for _, r := range spec.ProjectID {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", fmt.Errorf("invalid broker project ID %q", spec.ProjectID)
		}
	}
	return spec.ProjectID, nil
}

// guestRootOwnershipCommand verifies the project identity recorded outside the
// synchronized tree. Creation uses an atomic directory claim; incomplete and
// conflicting claims fail closed even if session listing is stale or two
// activations race.
func guestRootOwnershipCommand(spec SessionSpec, relative string, create bool) (string, error) {
	projectID, err := projectIdentity(spec)
	if err != nil {
		return "", err
	}
	owner := `.cloister/guest-root-owners/` + relative + `.owner`
	command := `owner="$HOME/` + owner + `"; `
	if create {
		command += `mkdir -p -- "$(dirname -- "$owner")" || exit 1; ` +
			`if mkdir -- "$owner" 2>/dev/null; then printf '%s\n' '` + projectID + `' > "$owner/project-id" || exit 1; fi; `
	}
	return command +
		`owner_id="$(cat -- "$owner/project-id" 2>/dev/null)" || { printf '%s\n' 'guest root ownership is incomplete; refusing destructive preparation' >&2; exit 1; }; ` +
		`[ "$owner_id" = '` + projectID + `' ] || { printf '%s\n' 'guest root is owned by a different project; refusing destructive preparation' >&2; exit 1; }`, nil
}

// GuestRootCommand returns a shell fragment for the generated safe guest path.
// When requireEmpty is true, an existing non-empty target is atomically moved
// to a persistent quarantine and activation stops for review because a new
// history must not merge two independently populated roots.
func GuestRootCommand(spec SessionSpec, requireEmpty bool) (string, error) {
	relative, err := guestRootRelative(spec)
	if err != nil {
		return "", err
	}
	claim, err := guestRootOwnershipCommand(spec, relative, true)
	if err != nil {
		return "", err
	}
	command := claim + `; target="$HOME/` + relative + `"; mkdir -p -- "$target" && test -d "$target" && test ! -L "$target"`
	if requireEmpty {
		quarantine := `.cloister/quarantine/guest-roots/` + relative + `.quarantine`
		command += ` || exit 1; quarantine="$HOME/` + quarantine + `"; ` +
			`if [ -e "$quarantine" ] || [ -L "$quarantine" ]; then printf 'guest root quarantine requires review: %s\n' "$quarantine" >&2; exit 1; fi; ` +
			`if [ -n "$(find "$target" -mindepth 1 -maxdepth 1 -print -quit)" ]; then mkdir -p -- "$(dirname -- "$quarantine")" && mv -- "$target" "$quarantine" || exit 1; printf 'non-empty guest root quarantined for review: %s\n' "$quarantine" >&2; exit 1; fi`
	}
	return command, nil
}

// GuestRootResetCommand clears and recreates a managed guest root only when an
// existing ownership record matches the requested project. Unclaimed roots and
// roots claimed by another project fail before deletion.
func GuestRootResetCommand(spec SessionSpec) (string, error) {
	relative, err := guestRootRelative(spec)
	if err != nil {
		return "", err
	}
	claim, err := guestRootOwnershipCommand(spec, relative, false)
	if err != nil {
		return "", err
	}
	return claim + `; target="$HOME/` + relative + `"; rm -rf -- "$target" && mkdir -p -- "$target" && test -d "$target" && test ! -L "$target"`, nil
}

// GuestRootRemoveCommand removes a synchronized tree only while holding its
// matching project claim. The claim is released after the tree is completely
// gone, which makes retrying an interrupted removal safe and idempotent.
func GuestRootRemoveCommand(spec SessionSpec) (string, error) {
	relative, err := guestRootRelative(spec)
	if err != nil {
		return "", err
	}
	claim, err := guestRootOwnershipCommand(spec, relative, true)
	if err != nil {
		return "", err
	}
	return claim + `; target="$HOME/` + relative + `"; rm -rf -- "$target" && rm -f -- "$owner/project-id" && rmdir -- "$owner"`, nil
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
