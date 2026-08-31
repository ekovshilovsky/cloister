package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SupportedMutagenVersion pins the CLI and human-readable list and status
// contracts used by this adapter. Cloister does not distribute or auto-install
// this binary.
const SupportedMutagenVersion = "0.18.1"

// CommandRunner is the subprocess seam used by unit tests.
type CommandRunner interface {
	Run(context.Context, string, []string, ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, binary string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	return cmd.CombinedOutput()
}

// Mutagen is a SyncBroker backed by one isolated per-user Mutagen daemon.
// Its fields contain only process configuration and no per-file handles or
// per-file indexes. Mutagen performs transient scans in its own process.
type Mutagen struct {
	Binary  string
	Runner  CommandRunner
	DataDir string
	SSHDir  string
	SSHPath string
	SCPPath string
	Log     io.Writer
}

// NewMutagen detects and version-checks the external Mutagen executable.
func NewMutagen() (*Mutagen, error) {
	binary, err := exec.LookPath("mutagen")
	if err != nil {
		return nil, missingMutagenError()
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("OpenSSH client not found in PATH; install OpenSSH before using workspace broker mode")
	}
	scpPath, err := exec.LookPath("scp")
	if err != nil {
		return nil, fmt.Errorf("OpenSSH scp client not found in PATH; install OpenSSH before using workspace broker mode")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory for Mutagen state: %w", err)
	}
	m := &Mutagen{
		Binary:  binary,
		Runner:  execCommandRunner{},
		DataDir: filepath.Join(home, ".cloister", "mutagen"),
		SSHDir:  filepath.Join(home, ".cloister", "run", "mutagen-ssh"),
		SSHPath: sshPath,
		SCPPath: scpPath,
		Log:     os.Stderr,
	}
	out, err := m.Runner.Run(context.Background(), m.Binary, os.Environ(), "version")
	if err != nil {
		return nil, fmt.Errorf("checking Mutagen version: %w", commandError(err, out))
	}
	version := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "Mutagen version "))
	if version != SupportedMutagenVersion {
		return nil, fmt.Errorf("unsupported Mutagen version %q, Cloister requires %s; install the approved version with `brew install mutagen-io/mutagen/mutagen`", version, SupportedMutagenVersion)
	}
	return m, nil
}

func missingMutagenError() error {
	return errors.New("Mutagen is required for workspace broker mode but was not found in PATH. Install the approved external dependency with `brew install mutagen-io/mutagen/mutagen`, then retry. Cloister will not install Mutagen automatically because its license and provenance require separate approval")
}

// Create creates a new session or resumes the existing stable project session.
func (m *Mutagen) Create(ctx context.Context, spec SessionSpec) error {
	policy, err := CompilePolicy(spec)
	if err != nil {
		return err
	}
	alias, err := m.prepareSSH(spec)
	if err != nil {
		return err
	}
	status, err := m.Status(ctx, spec)
	if err != nil {
		return err
	}
	if status.State != StateMissing {
		stored, readErr := os.ReadFile(m.policyPath(spec))
		current := hashPolicy(policy.Strings())
		policyStale := readErr != nil || strings.TrimSpace(string(stored)) != current
		guestStale := GuestRootDrifted(status, spec)
		if policyStale || guestStale {
			if guestStale {
				m.logf("Mutagen session %q guest root moved from %q to %q, terminating the stale session before recreation\n", spec.Name, status.GuestRoot, spec.GuestRoot)
			} else {
				m.logf("Mutagen session %q has a changed or unverified ignore policy, terminating the stale session before recreation\n", spec.Name)
			}
			if err := m.Terminate(ctx, spec); err != nil {
				return fmt.Errorf("ignore policy or guest root changed for Mutagen session %q; terminating the stale session failed, refusing to recreate it: %w", spec.Name, err)
			}
			m.logf("Terminated stale Mutagen session %q, creating a fresh synchronization history\n", spec.Name)
		} else {
			return m.Resume(ctx, spec)
		}
	}

	maxEntries := spec.MaxEntries
	if maxEntries == 0 {
		maxEntries = 250_000
	}
	maxFileSize := spec.MaxStagingFileSize
	if maxFileSize == "" {
		maxFileSize = "2 GiB"
	}
	args := []string{
		"sync", "create",
		"--name", spec.Name,
		"--sync-mode", "two-way-safe",
		"--symlink-mode", "portable",
		"--default-file-mode", "0644",
		"--default-directory-mode", "0755",
		"--max-entry-count", strconv.FormatUint(maxEntries, 10),
		"--max-staging-file-size", maxFileSize,
		"--no-global-configuration",
	}
	probeMode := spec.ProbeMode
	if probeMode == "" {
		probeMode = "assume"
	}
	args = append(args, "--probe-mode", probeMode)
	for _, pattern := range policy.Strings() {
		args = append(args, "--ignore", pattern)
	}
	args = append(args, spec.HostRoot, alias+":"+spec.GuestRoot)
	if _, err := m.run(ctx, args...); err != nil {
		return fmt.Errorf("creating Mutagen session %q: %w", spec.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(m.policyPath(spec)), 0o700); err != nil {
		_, _ = m.run(ctx, "sync", "terminate", spec.Name)
		return fmt.Errorf("creating broker policy state: %w", err)
	}
	if err := os.WriteFile(m.policyPath(spec), []byte(hashPolicy(policy.Strings())+"\n"), 0o600); err != nil {
		_, _ = m.run(ctx, "sync", "terminate", spec.Name)
		return fmt.Errorf("recording broker policy state: %w", err)
	}
	return nil
}

func (m *Mutagen) logf(format string, args ...any) {
	if m.Log != nil {
		fmt.Fprintf(m.Log, format, args...)
	}
}

// ReconcileProfile removes sessions that are no longer part of a complete
// profile workspace collection. It fails closed before callers activate any
// desired session when listing, health verification, flush, or termination is
// uncertain.
func (m *Mutagen) ReconcileProfile(ctx context.Context, profile string, desired []SessionSpec) error {
	profileID := sanitize(profile)
	desiredNames := make(map[string]struct{}, len(desired))
	for _, spec := range desired {
		sessionProfile, ok := splitCloisterSessionName(spec.Name)
		if !ok || sessionProfile != profileID {
			return fmt.Errorf("desired session %q is not in the exact Cloister profile namespace %q", spec.Name, profileID)
		}
		desiredNames[spec.Name] = struct{}{}
	}

	output, err := m.run(ctx, "sync", "list")
	if err != nil {
		return fmt.Errorf("listing Mutagen sessions for profile %q: %w", profile, err)
	}
	names, err := parseMutagenSessionNames(output)
	if err != nil {
		return fmt.Errorf("listing Mutagen sessions for profile %q: %w", profile, err)
	}
	for _, name := range names {
		sessionProfile, managed := splitCloisterSessionName(name)
		if !managed || sessionProfile != profileID {
			continue
		}
		if _, keep := desiredNames[name]; keep {
			continue
		}
		spec := SessionSpec{Profile: profile, Name: name}
		if err := m.reconcileObsoleteSession(ctx, spec); err != nil {
			return fmt.Errorf("reconciling obsolete Mutagen session %q: %w", name, err)
		}
	}
	return nil
}

func (m *Mutagen) reconcileObsoleteSession(ctx context.Context, spec SessionSpec) error {
	status, err := m.Status(ctx, spec)
	if err != nil {
		return err
	}
	if status.State == StateMissing {
		return nil
	}
	if err := status.Clean(); err != nil {
		return err
	}
	if status.State == StateActive {
		if err := m.Flush(ctx, spec); err != nil {
			return err
		}
		status, err = m.Status(ctx, spec)
		if err != nil {
			return err
		}
		if status.State == StateMissing {
			return nil
		}
		if err := status.Clean(); err != nil {
			return err
		}
		if status.State != StateActive {
			return fmt.Errorf("synchronization session changed state to %q after flush", status.State)
		}
	} else if status.State != StatePaused {
		return fmt.Errorf("synchronization session has unknown state %q", status.State)
	}
	if err := m.Terminate(ctx, spec); err != nil {
		return err
	}
	return nil
}

func parseMutagenSessionNames(output []byte) ([]string, error) {
	var names []string
	seen := make(map[string]struct{})
	for _, raw := range bytes.Split(output, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if len(line) < len("Name:") || !strings.EqualFold(line[:len("Name:")], "Name:") {
			continue
		}
		name := strings.TrimSpace(line[len("Name:"):])
		if name == "" {
			return nil, fmt.Errorf("Mutagen returned an empty session name")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("Mutagen returned duplicate session name %q", name)
		}
		if strings.HasPrefix(name, "cloister-") {
			if _, ok := splitCloisterSessionName(name); !ok {
				return nil, fmt.Errorf("Mutagen returned malformed Cloister session name %q", name)
			}
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 && !isMissingOutput(output) {
		return nil, fmt.Errorf("Mutagen returned an unrecognized session list; refusing obsolete-session reconciliation")
	}
	return names, nil
}

func splitCloisterSessionName(name string) (string, bool) {
	const prefix = "cloister-"
	const projectIDLength = 24
	if !strings.HasPrefix(name, prefix) || len(name) <= len(prefix)+1+projectIDLength {
		return "", false
	}
	projectSeparator := len(name) - projectIDLength - 1
	if name[projectSeparator] != '-' {
		return "", false
	}
	profileID := name[len(prefix):projectSeparator]
	if profileID == "" || sanitize(profileID) != profileID {
		return "", false
	}
	for _, character := range name[projectSeparator+1:] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "", false
		}
	}
	return profileID, true
}

func (m *Mutagen) Flush(ctx context.Context, spec SessionSpec) error {
	if _, err := m.run(ctx, "sync", "flush", spec.Name); err != nil {
		return fmt.Errorf("flushing Mutagen session %q: %w", spec.Name, err)
	}
	return nil
}

func (m *Mutagen) Pause(ctx context.Context, spec SessionSpec) error {
	if _, err := m.run(ctx, "sync", "pause", spec.Name); err != nil {
		return fmt.Errorf("pausing Mutagen session %q: %w", spec.Name, err)
	}
	return nil
}

func (m *Mutagen) Resume(ctx context.Context, spec SessionSpec) error {
	if _, err := m.run(ctx, "sync", "resume", spec.Name); err != nil {
		return fmt.Errorf("resuming Mutagen session %q: %w", spec.Name, err)
	}
	return nil
}

func (m *Mutagen) Terminate(ctx context.Context, spec SessionSpec) error {
	status, err := m.Status(ctx, spec)
	if err != nil {
		return err
	}
	if status.State == StateMissing {
		return nil
	}
	if err := status.Clean(); err != nil {
		return fmt.Errorf("refusing to terminate unclean Mutagen session %q: %w", spec.Name, err)
	}
	if _, err := m.run(ctx, "sync", "terminate", spec.Name); err != nil {
		return fmt.Errorf("terminating Mutagen session %q: %w", spec.Name, err)
	}
	if err := os.Remove(m.policyPath(spec)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing broker policy state: %w", err)
	}
	return nil
}

func (m *Mutagen) policyPath(spec SessionSpec) string {
	return filepath.Join(m.DataDir, "cloister-policies", spec.Name+".sha256")
}

func hashPolicy(patterns []string) string {
	hash := sha256.New()
	for _, pattern := range patterns {
		hash.Write([]byte(pattern))
		hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func (m *Mutagen) Status(ctx context.Context, spec SessionSpec) (Status, error) {
	out, err := m.run(ctx, "sync", "list", "--long", spec.Name)
	if err != nil {
		if isMissingSessionError(err, out) {
			return Status{State: StateMissing}, nil
		}
		return Status{}, fmt.Errorf("reading Mutagen session %q status: %w", spec.Name, err)
	}
	return parseMutagenStatus(out)
}

func (m *Mutagen) run(ctx context.Context, args ...string) ([]byte, error) {
	if m == nil || m.Binary == "" {
		return nil, missingMutagenError()
	}
	if m.Runner == nil {
		return nil, fmt.Errorf("Mutagen command runner is required")
	}
	if err := os.MkdirAll(m.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating private Mutagen state: %w", err)
	}
	env := append([]string(nil), os.Environ()...)
	env = append(env, "MUTAGEN_DATA_DIRECTORY="+m.DataDir, "MUTAGEN_SSH_PATH="+m.SSHDir)
	out, err := m.Runner.Run(ctx, m.Binary, env, args...)
	if err != nil {
		return out, commandError(err, out)
	}
	return out, nil
}

func commandError(err error, output []byte) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, message)
}

func isMissingSessionError(err error, output []byte) bool {
	type exitCoder interface {
		ExitCode() int
	}
	var exitError exitCoder
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		return false
	}
	lower := strings.ToLower(string(output))
	return strings.Contains(lower, "did not match any sessions") ||
		strings.Contains(lower, "unable to locate requested sessions")
}

func isMissingOutput(output []byte) bool {
	lower := strings.ToLower(string(output))
	return strings.Contains(lower, "no sessions found") || strings.Contains(lower, "no matching sessions") || strings.Contains(lower, "unable to find")
}

// mutagenProgressStatuses are the Mutagen 0.18.1 status descriptions a healthy
// session reports while it makes progress. A session that has just been created
// or has just received a change passes through them before it settles back to
// watching, so treating them as problems fails a barrier that had already
// completed its blocking flush.
var mutagenProgressStatuses = []string{
	"Watching for changes",
	"Scanning files",
	"Reconciling changes",
	"Staging files on alpha",
	"Staging files on beta",
	"Applying changes",
	"Saving archive",
}

// mutagenProblemHeadings are the endpoint problem sections Mutagen reports.
// Mutagen 0.18.1 splits them into scan and transition problems; the combined
// headings remain recognized for compatibility with other reporting shapes.
var mutagenProblemHeadings = []string{
	"scan problems:",
	"transition problems:",
	"alpha problems:",
	"beta problems:",
}

// isMutagenProgressStatus reports whether a status description is a known
// healthy progress state. Every other description, including halted, connecting,
// disconnected, waiting for rescan after a scan failure, and any state a future
// Mutagen release adds, fails closed as a problem.
func isMutagenProgressStatus(description string) bool {
	description = strings.TrimSpace(description)
	for _, progress := range mutagenProgressStatuses {
		if strings.EqualFold(description, progress) {
			return true
		}
	}
	return false
}

// hasMutagenEndpointProblem reports whether a lowercased status line opens an
// endpoint problem section. Mutagen prints these headings only when problems
// exist and lists the individual problems on the following lines, so the
// heading itself is the signal unless it carries an explicit empty count.
func hasMutagenEndpointProblem(lower string) bool {
	lower = strings.TrimSpace(lower)
	for _, heading := range mutagenProblemHeadings {
		if !strings.HasPrefix(lower, heading) {
			continue
		}
		value := strings.TrimSpace(lower[len(heading):])
		return value != "none" && value != "0"
	}
	return false
}

func parseMutagenStatus(output []byte) (Status, error) {
	if isMissingOutput(output) {
		return Status{State: StateMissing}, nil
	}
	status := Status{State: StateActive}
	lines := bytes.Split(output, []byte("\n"))
	foundSessions := 0
	// A paused session reports both endpoints as unconnected because Mutagen
	// drops its transports while paused. Endpoint connectivity is therefore
	// only conclusive once the reported session state is known.
	var disconnectedEndpoints []string
	endpoint := ""
	for _, raw := range lines {
		line := strings.TrimSpace(string(raw))
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "name:"):
			foundSessions++
			endpoint = ""
		case strings.HasPrefix(lower, "alpha:"):
			endpoint = "alpha"
		case strings.HasPrefix(lower, "beta:"):
			endpoint = "beta"
		case endpoint == "beta" && strings.HasPrefix(lower, "url:"):
			status.GuestRoot = mutagenURLGuestRoot(strings.TrimSpace(line[len("URL:"):]))
			endpoint = ""
		case strings.HasPrefix(lower, "status:"):
			status.Description = strings.TrimSpace(line[len("Status:"):])
			switch {
			case strings.Contains(lower, "paused"):
				status.State = StatePaused
			case isMutagenProgressStatus(status.Description):
				// A healthy session cycles through these states, so progress
				// keeps the session active without clearing a problem that an
				// earlier endpoint line already recorded.
			default:
				status.State = StateProblem
				status.Problems = append(status.Problems, status.Description)
			}
		case strings.Contains(lower, "connection state: disconnected"), strings.Contains(lower, "connected: no"):
			disconnectedEndpoints = append(disconnectedEndpoints, line)
		case strings.HasPrefix(lower, "last error:"):
			status.State = StateProblem
			status.Problems = append(status.Problems, strings.TrimSpace(line[len("Last error:"):]))
		case strings.HasPrefix(lower, "conflicts:"):
			value := strings.TrimSpace(line[len("Conflicts:"):])
			if value == "" {
				status.ConflictCount++
			} else if !strings.EqualFold(value, "none") {
				count, err := strconv.Atoi(value)
				if err != nil || count < 0 {
					return Status{}, fmt.Errorf("Mutagen returned an invalid conflict count %q", value)
				}
				status.ConflictCount += count
			}
		case hasMutagenEndpointProblem(lower):
			status.State = StateProblem
			status.Problems = append(status.Problems, line)
		}
	}
	if foundSessions == 0 {
		return Status{}, fmt.Errorf("Mutagen returned an unrecognized status response; refusing to assume the session is clean")
	}
	if foundSessions != 1 {
		return Status{}, fmt.Errorf("Mutagen returned %d sessions for one project name; refusing ambiguous lifecycle operations", foundSessions)
	}
	if status.Description == "" {
		return Status{}, fmt.Errorf("Mutagen status response omitted session state; refusing to assume the session is clean")
	}
	if len(disconnectedEndpoints) > 0 && status.State != StatePaused {
		status.State = StateProblem
		status.Problems = append(status.Problems, disconnectedEndpoints...)
	}
	return status, nil
}

func mutagenURLGuestRoot(url string) string {
	index := strings.Index(url, "~/workspaces/")
	if index < 0 {
		return ""
	}
	return url[index:]
}

func (m *Mutagen) prepareSSH(spec SessionSpec) (string, error) {
	if m.SSHDir == "" || m.SSHPath == "" || m.SCPPath == "" {
		return "", fmt.Errorf("Mutagen SSH wrapper configuration is incomplete")
	}
	if len(spec.ProjectID) < 16 {
		return "", fmt.Errorf("invalid broker project ID %q", spec.ProjectID)
	}
	if strings.ContainsAny(spec.SSH.HostAlias+spec.SSH.Host+spec.SSH.User+spec.SSH.ConfigFile+spec.SSH.KeyFile, "\r\n") {
		return "", fmt.Errorf("invalid newline in broker SSH configuration")
	}
	configs := filepath.Join(m.SSHDir, "configs")
	if err := os.MkdirAll(configs, 0o700); err != nil {
		return "", fmt.Errorf("creating Mutagen SSH config directory: %w", err)
	}
	mainConfig := "Include " + sshConfigQuote(filepath.Join(configs, "*")) + "\n"
	if err := os.WriteFile(filepath.Join(m.SSHDir, "config"), []byte(mainConfig), 0o600); err != nil {
		return "", fmt.Errorf("writing Mutagen SSH config: %w", err)
	}
	if err := writeWrapper(filepath.Join(m.SSHDir, "ssh"), m.SSHPath, filepath.Join(m.SSHDir, "config")); err != nil {
		return "", err
	}
	if err := writeWrapper(filepath.Join(m.SSHDir, "scp"), m.SCPPath, filepath.Join(m.SSHDir, "config")); err != nil {
		return "", err
	}

	alias := spec.SSH.HostAlias
	var fragment string
	if spec.SSH.ConfigFile != "" && alias != "" {
		fragment = "Include " + sshConfigQuote(spec.SSH.ConfigFile) + "\n"
	} else {
		if spec.SSH.Host == "" || spec.SSH.User == "" {
			return "", fmt.Errorf("profile %q has incomplete SSH access for broker mode", spec.Profile)
		}
		alias = "cloister-sync-" + spec.ProjectID[:16]
		fragment = "Host " + alias + "\n  HostName " + spec.SSH.Host + "\n  User " + spec.SSH.User + "\n"
		if spec.SSH.KeyFile != "" {
			fragment += "  IdentityFile " + sshConfigQuote(spec.SSH.KeyFile) + "\n  IdentitiesOnly yes\n"
		}
		fragment += "  StrictHostKeyChecking accept-new\n  UserKnownHostsFile " + sshConfigQuote(filepath.Join(m.SSHDir, "known_hosts")) + "\n"
	}
	fragmentPath := filepath.Join(configs, spec.ProjectID+".conf")
	if err := os.WriteFile(fragmentPath, []byte(fragment), 0o600); err != nil {
		return "", fmt.Errorf("writing Mutagen SSH profile: %w", err)
	}
	return alias, nil
}

func writeWrapper(path, executable, config string) error {
	if strings.ContainsAny(executable+config, "\r\n") {
		return fmt.Errorf("invalid newline in SSH wrapper path")
	}
	script := "#!/bin/sh\nexec " + shellQuote(executable) + " -F " + shellQuote(config) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return fmt.Errorf("writing Mutagen SSH wrapper: %w", err)
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func sshConfigQuote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return "\"" + value + "\""
}
