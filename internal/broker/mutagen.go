// Proprietary and confidential. All rights reserved.

package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	brokerignore "cloister.io/internal/broker/ignore"
)

// SupportedMutagenVersion pins the CLI and human-readable status contract used
// by this adapter. Cloister does not distribute or auto-install this binary.
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
	policy, err := brokerignore.Compile(spec.HostRoot, spec.Ignore)
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
		if readErr != nil || strings.TrimSpace(string(stored)) != current {
			return fmt.Errorf("ignore policy changed or is unverified for Mutagen session %q; refusing to resume with stale exposure rules. Perform a clean rebuild to create a new synchronization history", spec.Name)
		}
		return m.Resume(ctx, spec)
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

func parseMutagenStatus(output []byte) (Status, error) {
	if isMissingOutput(output) {
		return Status{State: StateMissing}, nil
	}
	status := Status{State: StateActive}
	lines := bytes.Split(output, []byte("\n"))
	foundSessions := 0
	for _, raw := range lines {
		line := strings.TrimSpace(string(raw))
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "name:"):
			foundSessions++
		case strings.HasPrefix(lower, "status:"):
			status.Description = strings.TrimSpace(line[len("Status:"):])
			if strings.Contains(lower, "paused") {
				status.State = StatePaused
			} else if !strings.Contains(lower, "watching for changes") {
				status.State = StateProblem
				status.Problems = append(status.Problems, status.Description)
			}
		case strings.Contains(lower, "connection state: disconnected"), strings.Contains(lower, "connected: no"):
			status.State = StateProblem
			status.Problems = append(status.Problems, line)
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
		case strings.HasPrefix(lower, "alpha problems:") && !strings.Contains(lower, "none"), strings.HasPrefix(lower, "beta problems:") && !strings.Contains(lower, "none"):
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
	return status, nil
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
