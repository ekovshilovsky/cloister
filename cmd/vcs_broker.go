package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/processidentity"
	"cloister.io/internal/tunnel"
	"cloister.io/internal/vcsbroker"
	"cloister.io/internal/vm"
	"github.com/spf13/cobra"
)

const (
	vcsBrokerGuestPort             = 49231
	vcsBrokerLockWait              = 12 * time.Second
	vcsBrokerStartupWait           = 30 * time.Second
	vcsBrokerDrainWait             = 10 * time.Minute
	vcsBrokerShutdownWait          = vcsBrokerDrainWait + 5*time.Second
	vcsBrokerProgressEvery         = 5 * time.Second
	vcsBrokerVMCheckEvery          = 15 * time.Second
	vcsBrokerVMMisses              = 3
	vcsBrokerGuestVerifyTicks      = 4
	vcsBrokerMaxTransitionAttempts = 5
)

var vcsBrokerTransitionRetryBase = 5 * time.Second
var vcsBrokerGenerationIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type vcsBrokerServiceConfig struct {
	OwnerID         string                 `json:"owner_id"`
	GenerationID    string                 `json:"generation_id"`
	GenerationOrder uint64                 `json:"generation_order"`
	Profile         string                 `json:"profile"`
	Backend         string                 `json:"backend"`
	GuestHome       string                 `json:"guest_home"`
	Specs           []broker.SessionSpec   `json:"specs"`
	Workspace       config.WorkspaceConfig `json:"workspace"`
	ConfigHash      string                 `json:"config_hash"`
	BuildID         string                 `json:"build_id"`
	StatePath       string                 `json:"state_path"`
	ReadyPath       string                 `json:"ready_path"`
	RepairPath      string                 `json:"repair_path"`
	DrainPath       string                 `json:"drain_path"`
	ActivityPath    string                 `json:"activity_path"`
	TransitionPath  string                 `json:"transition_path"`
	RequestPath     string                 `json:"request_path"`
	SpoolDir        string                 `json:"spool_dir"`
	ConfigPath      string                 `json:"config_path"`
	LogPath         string                 `json:"log_path"`
	DrainWait       time.Duration          `json:"drain_wait"`
	RestartDaemon   bool                   `json:"restart_daemon,omitempty"`
}

type vcsBrokerReady struct {
	OwnerID      string                 `json:"owner_id,omitempty"`
	GenerationID string                 `json:"generation_id,omitempty"`
	BrokerPID    int                    `json:"broker_pid,omitempty"`
	State        vcsbroker.ServiceState `json:"state,omitempty"`
	Ready        bool                   `json:"ready,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

type vcsBrokerRepairReady struct {
	OwnerID      string `json:"owner_id,omitempty"`
	GenerationID string `json:"generation_id,omitempty"`
	TunnelPID    int    `json:"tunnel_pid,omitempty"`
	Error        string `json:"error,omitempty"`
}

type vcsBrokerDrainReport struct {
	OwnerID      string                    `json:"owner_id"`
	GenerationID string                    `json:"generation_id"`
	Commands     []vcsbroker.ActiveCommand `json:"commands"`
}

type vcsBrokerDrainTimeoutError struct {
	Commands []vcsbroker.ActiveCommand
}

func (e *vcsBrokerDrainTimeoutError) Error() string {
	return fmt.Sprintf("VCS broker drain exceeded %s; interrupted %s", vcsBrokerDrainWait, describeVCSBrokerCommands(e.Commands))
}

func describeVCSBrokerCommands(commands []vcsbroker.ActiveCommand) string {
	if len(commands) == 0 {
		return "active commands whose request details are not yet available"
	}
	details := make([]string, 0, len(commands))
	for _, command := range commands {
		argv := append([]string{command.Tool}, command.Args...)
		details = append(details, fmt.Sprintf("%q in project %q", strings.Join(argv, " "), command.Project))
	}
	return strings.Join(details, "; ")
}

type vcsBrokerHealth struct {
	Host          vcsbroker.HostProbeStatus
	Tunnel        bool
	ProcessAlive  bool
	Process       processidentity.Observation
	TunnelProcess processidentity.Observation
}

type vcsBrokerActivity struct {
	OwnerID      string                    `json:"owner_id"`
	GenerationID string                    `json:"generation_id"`
	Revision     uint64                    `json:"revision"`
	Draining     bool                      `json:"draining"`
	Commands     []vcsbroker.ActiveCommand `json:"commands"`
}

type vcsBrokerTransitionStatus struct {
	OwnerID      string                    `json:"owner_id"`
	GenerationID string                    `json:"generation_id"`
	Attempt      int                       `json:"attempt"`
	State        string                    `json:"state"`
	Error        string                    `json:"error,omitempty"`
	RetryAt      time.Time                 `json:"retry_at,omitempty"`
	Commands     []vcsbroker.ActiveCommand `json:"commands,omitempty"`
}

type vcsBrokerProcessInspector interface {
	ProcessAlive(vcsbroker.ServiceState) bool
}

type vcsBrokerProcessObserver interface {
	ProcessObservation(vcsbroker.ServiceState) processidentity.Observation
}

type vcsBrokerRuntime interface {
	Inspect(vm.Backend, string, vcsbroker.ServiceState) vcsBrokerHealth
	RequestTunnelRepair(vcsbroker.ServiceState) error
	RequestRestart(vcsBrokerServiceConfig, vcsbroker.ServiceState) error
	RequestShutdown(vcsbroker.ServiceState) error
	Start(vcsBrokerServiceConfig) (vcsbroker.ServiceState, error)
	Stop(vm.Backend, string, vcsbroker.ServiceState) error
	ForceStop(vm.Backend, string, vcsbroker.ServiceState) error
}

type vcsBrokerManager struct {
	stateDir string
	lockWait time.Duration
	runtime  vcsBrokerRuntime
	newID    func() (string, error)
	buildID  string
}

// ensureVCSBrokerFn is the command-level seam for tests whose concern is not
// the standalone service composition.
var ensureVCSBrokerFn = ensureVCSBroker
var stopVCSBrokerFn = stopVCSBroker
var startVCSBrokerTunnelFn = tunnel.StartOwnedReverseForward
var stopVCSBrokerTunnelFn = tunnel.StopOwnedReverseForward
var retireLegacyVCSBrokerTunnelFn = tunnel.RetireLegacyReverseForward
var deployVCSBrokerGuestFn = vcsbroker.DeployGuest
var probeVCSBrokerGuestFn = vcsbroker.ProbeGuest
var probeVCSBrokerGuestWithRetryFn = vcsbroker.ProbeGuestWithRetry
var ensureVCSBrokerGuestInstallationFn = vcsbroker.EnsureGuestInstallation
var newVCSBrokerHostRunnerFn = newVCSBrokerHostRunner
var startVCSBrokerReplacementFn = (realVCSBrokerRuntime{}).Start
var launchVCSBrokerEnsureFn = launchVCSBrokerEnsure
var vcsBrokerExecutableFn = os.Executable
var observeVCSBrokerProcessFn = processidentity.Observe
var vcsBrokerProcessCommandFn = func(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return strings.TrimSpace(string(out)), err
}

var errVCSBrokerEnsureAlreadyRunning = errors.New("VCS broker ensure is already running")

func ensureVCSBrokerWithWarning(backend vm.Backend, profile string, p *config.Profile) {
	done := make(chan error, 1)
	go func() { done <- ensureVCSBrokerFn(backend, profile, p) }()
	select {
	case err := <-done:
		if err != nil {
			printVCSBrokerEnsureWarning(profile, err)
		}
		return
	case <-time.After(time.Second):
		fmt.Fprintf(os.Stderr, "Waiting for VCS broker for profile %q...\n", profile)
	}
	if err := <-done; err != nil {
		printVCSBrokerEnsureWarning(profile, err)
	}
}

func printVCSBrokerEnsureWarning(profile string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: VCS broker for profile %q: %v; VM access will continue; retry with 'cloister repair %s'\n", profile, err, profile)
	}
}

func ensureVCSBrokerAsyncWithWarning(profile string) {
	if outcome := readDetachedVCSBrokerEnsureOutcome(profile); outcome != nil {
		printVCSBrokerEnsureWarning(profile, outcome)
	}
	if err := launchVCSBrokerEnsureFn(profile); err != nil {
		if errors.Is(err, errVCSBrokerEnsureAlreadyRunning) {
			fmt.Fprintf(os.Stderr, "VCS broker check for profile %q is already running in the background.\n", profile)
			return
		}
		printVCSBrokerEnsureWarning(profile, err)
		return
	}
	fmt.Fprintf(os.Stderr, "VCS broker check for profile %q is continuing in the background.\n", profile)
}

func launchVCSBrokerEnsure(profile string) error {
	configDir, err := config.ConfigDir()
	if err != nil {
		return err
	}
	store := vcsbroker.NewStateStore(filepath.Join(configDir, "state"), profile, vcsBrokerLockWait)
	locked, acquired, err := store.TryLock()
	if err != nil {
		return err
	}
	if !acquired {
		return errVCSBrokerEnsureAlreadyRunning
	}
	defer locked.Close()
	state, err := locked.Load()
	if err != nil {
		return err
	}
	if processidentity.Matches(state.EnsureHelperPID, state.EnsureHelperIdentity) {
		return errVCSBrokerEnsureAlreadyRunning
	}
	executable, err := vcsBrokerExecutableFn()
	if err != nil {
		return fmt.Errorf("locating cloister executable: %w", err)
	}
	command := exec.Command(executable, "vcs-broker", "ensure", profile)
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("starting background VCS broker ensure: %w", err)
	}
	identity, err := processidentity.Read(command.Process.Pid)
	if err != nil {
		_ = command.Process.Release()
		return fmt.Errorf("capturing background VCS broker ensure identity: %w", err)
	}
	state.EnsureHelperPID = command.Process.Pid
	state.EnsureHelperIdentity = identity
	state.EnsureError = ""
	state.EnsureCompletedAt = time.Time{}
	if err := locked.Save(state); err != nil {
		_ = processidentity.Kill(command.Process.Pid, identity)
		_ = command.Process.Release()
		return fmt.Errorf("recording background VCS broker ensure: %w", err)
	}
	return command.Process.Release()
}

func runVCSBrokerEnsure(profile string) error {
	runErr := runVCSBrokerEnsureWork(profile)
	if err := recordDetachedVCSBrokerEnsureOutcome(profile, runErr); err != nil {
		return err
	}
	return nil
}

func runVCSBrokerEnsureWork(profile string) error {
	configPath, err := config.ConfigPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	p, ok := cfg.Profiles[profile]
	if !ok {
		return fmt.Errorf("profile %q not found", profile)
	}
	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}
	if !backend.IsRunning(profile) {
		return fmt.Errorf("profile %q is not running", profile)
	}
	if err := ensureVCSBrokerFn(backend, profile, p); err != nil {
		return err
	}
	return nil
}

func recordDetachedVCSBrokerEnsureOutcome(profile string, runErr error) error {
	configDir, err := config.ConfigDir()
	if err != nil {
		return err
	}
	store := vcsbroker.NewStateStore(filepath.Join(configDir, "state"), profile, vcsBrokerLockWait)
	locked, err := store.Lock(context.Background())
	if err != nil {
		return err
	}
	defer locked.Close()
	state, err := locked.Load()
	if err != nil {
		return err
	}
	identity, identityErr := processidentity.Read(os.Getpid())
	if identityErr == nil && state.EnsureHelperPID == os.Getpid() && state.EnsureHelperIdentity == identity {
		state.EnsureHelperPID = 0
		state.EnsureHelperIdentity = processidentity.Identity{}
	}
	state.EnsureCompletedAt = time.Now()
	state.EnsureError = ""
	if runErr != nil {
		state.EnsureError = runErr.Error()
	}
	if err := locked.Save(state); err != nil {
		return err
	}
	if state.LogPath != "" {
		if logFile, logErr := os.OpenFile(state.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); logErr == nil {
			if runErr == nil {
				_, _ = fmt.Fprintf(logFile, "%s detached ensure completed successfully\n", state.EnsureCompletedAt.Format(time.RFC3339))
			} else {
				_, _ = fmt.Fprintf(logFile, "%s detached ensure failed: %v\n", state.EnsureCompletedAt.Format(time.RFC3339), runErr)
			}
			_ = logFile.Close()
		}
	}
	return nil
}

func readDetachedVCSBrokerEnsureOutcome(profile string) error {
	configDir, err := config.ConfigDir()
	if err != nil {
		return nil
	}
	store := vcsbroker.NewStateStore(filepath.Join(configDir, "state"), profile, vcsBrokerLockWait)
	state, err := vcsbroker.ReadServiceState(store.StatePath)
	if err != nil || state.EnsureError == "" {
		return nil
	}
	return fmt.Errorf("background ensure failed at %s: %s", state.EnsureCompletedAt.Format(time.RFC3339), state.EnsureError)
}

func ensureVCSBroker(backend vm.Backend, profile string, p *config.Profile) error {
	manager, err := newVCSBrokerManager()
	if err != nil {
		return err
	}
	if !workspaceProvider(p).IsBroker() {
		return manager.retire(backend, profile)
	}
	backendName, err := vm.ResolveBackendName(p.Backend)
	if err != nil {
		return err
	}
	return manager.ensure(backend, profile, backendName, p)
}

// retire asks a service made obsolete by a workspace-mode change to drain in
// its own process. The ensure caller never waits for that potentially long
// drain, and the daemon removes its state after it finishes.
func (m *vcsBrokerManager) retire(backend vm.Backend, profile string) error {
	store := vcsbroker.NewStateStore(m.stateDir, profile, m.lockWait)
	locked, err := store.Lock(context.Background())
	if err != nil {
		return err
	}
	defer locked.Close()
	state, err := m.adoptReadyVCSBrokerGeneration(backend, profile, store.StatePath, locked)
	if err != nil {
		return err
	}
	setVCSBrokerStatePaths(m.stateDir, store.StatePath, &state)
	if state.OwnerID == "" {
		return locked.Remove()
	}
	state.StatePath = store.StatePath
	state.Phase = "retiring"
	if err := locked.Save(state); err != nil {
		return err
	}
	if err := m.runtime.RequestShutdown(state); err == nil {
		return nil
	}
	if runtimeVCSBrokerProcessAlive(m.runtime, state) {
		return fmt.Errorf("obsolete VCS broker is still running; graceful shutdown remains pending")
	}
	if err := m.runtime.ForceStop(backend, profile, state); err != nil {
		return fmt.Errorf("stopping obsolete VCS broker: %w", err)
	}
	return locked.Remove()
}

func stopVCSBroker(backend vm.Backend, profile string) error {
	manager, err := newVCSBrokerManager()
	if err != nil {
		return err
	}
	return manager.stop(backend, profile)
}

func stopVCSBrokerForLifecycle(backend vm.Backend, profile string) error {
	err := stopVCSBrokerFn(backend, profile)
	var drainErr *vcsBrokerDrainTimeoutError
	if errors.As(err, &drainErr) {
		fmt.Fprintf(os.Stderr, "warning: %v; continuing VM lifecycle teardown\n", drainErr)
		return nil
	}
	// Ownership failures retain their actionable PID/command guidance and stop
	// destructive delete, rebuild, and reset callers before VM teardown.
	return err
}

func newVCSBrokerManager() (*vcsBrokerManager, error) {
	configDir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	buildID := currentVCSBrokerBuildID()
	return &vcsBrokerManager{
		stateDir: filepath.Join(configDir, "state"),
		lockWait: vcsBrokerLockWait,
		runtime:  realVCSBrokerRuntime{},
		newID:    newVCSBrokerID,
		buildID:  buildID,
	}, nil
}

func (m *vcsBrokerManager) ensure(backend vm.Backend, profile, backendName string, p *config.Profile) error {
	specs, err := brokerSessionSpecs(backend, profile, p)
	if err != nil {
		return err
	}
	guestHome, err := resolveVCSBrokerGuestHome(backend, profile)
	if err != nil {
		return err
	}
	configHash, err := vcsBrokerConfigHash(guestHome, p.Workspace, specs)
	if err != nil {
		return err
	}
	store := vcsbroker.NewStateStore(m.stateDir, profile, m.lockWait)
	locked, err := store.Lock(context.Background())
	if err != nil {
		return err
	}
	defer locked.Close()

	state, err := m.adoptReadyVCSBrokerGeneration(backend, profile, store.StatePath, locked)
	if err != nil {
		return err
	}
	setVCSBrokerStatePaths(m.stateDir, store.StatePath, &state)
	if validVCSBrokerState(state) {
		health := m.runtime.Inspect(backend, profile, state)
		if health.Host == vcsbroker.HostProbeDraining {
			return fmt.Errorf("VCS broker is stopping or restarting; retry with 'cloister repair %s'", profile)
		}
		if health.Host == vcsbroker.HostProbeHealthy {
			if state.ConfigHash != configHash || state.BuildID != m.buildID || state.Phase == "unhealthy-replacement-pending" {
				desired := serviceConfigForExisting(state, profile, backendName, guestHome, p.Workspace, specs, configHash, m.buildID)
				if state.BuildID != m.buildID || state.Phase == "unhealthy-replacement-pending" {
					generationID, idErr := m.newID()
					if idErr != nil {
						return fmt.Errorf("creating VCS broker generation identity: %w", idErr)
					}
					desired = replacementVCSBrokerServiceConfig(m.stateDir, desired, generationID)
				}
				if err := m.runtime.RequestRestart(desired, state); err != nil {
					return fmt.Errorf("requesting deferred VCS broker transition: %w", err)
				}
				return fmt.Errorf("VCS broker configuration or build transition is pending; current mappings remain available")
			}
			if health.Tunnel {
				return readVCSBrokerTransitionWarning(state)
			}
			if health.TunnelProcess.State == processidentity.Unverifiable && health.TunnelProcess.Err != nil {
				return unverifiableVCSBrokerTunnelError(state.TunnelPID, health.TunnelProcess.Err)
			}
			if err := m.runtime.RequestTunnelRepair(state); err != nil {
				return fmt.Errorf("requesting VCS broker tunnel repair: %w", err)
			}
			return fmt.Errorf("VCS broker tunnel repair requested; retry with 'cloister repair %s'", profile)
		}
		observation := vcsBrokerHealthProcessObservation(health)
		if observation.State == processidentity.Unverifiable {
			return unverifiableVCSBrokerProcessError(state.BrokerPID, observation.Err)
		}
		if observation.State == processidentity.Ours {
			desired := serviceConfigForExisting(state, profile, backendName, guestHome, p.Workspace, specs, configHash, m.buildID)
			generationID, idErr := m.newID()
			if idErr != nil {
				return fmt.Errorf("creating VCS broker generation identity: %w", idErr)
			}
			desired = replacementVCSBrokerServiceConfig(m.stateDir, desired, generationID)
			desired.RestartDaemon = true
			state.Phase = "unhealthy-replacement-pending"
			if err := locked.Save(state); err != nil {
				return err
			}
			if err := m.runtime.RequestRestart(desired, state); err != nil {
				return fmt.Errorf("VCS broker host probe failed, but its owned process is alive; refusing forced replacement: %w", err)
			}
			return fmt.Errorf("VCS broker host probe failed; graceful replacement requested%s", describeVCSBrokerActivity(state))
		}
	}
	if state.OwnerID != "" {
		observation := runtimeVCSBrokerProcessObservation(m.runtime, state)
		if observation.State == processidentity.Ours {
			return fmt.Errorf("VCS broker state is incomplete but its owned process is alive; refusing forced replacement")
		}
		if observation.State == processidentity.Unverifiable {
			return unverifiableVCSBrokerProcessError(state.BrokerPID, observation.Err)
		}
		stopErr := m.runtime.ForceStop(backend, profile, state)
		if stopErr != nil {
			return fmt.Errorf("stopping stale VCS broker: %w", stopErr)
		}
		if err := locked.Remove(); err != nil {
			return err
		}
	}
	legacyRetired, err := retireLegacyVCSBrokerTunnelFn(profile, "vcs-broker", vcsBrokerGuestPort, backend.SSHConfig(profile))
	if err != nil {
		return fmt.Errorf("migrating legacy VCS broker tunnel: %w", err)
	}
	if legacyRetired {
		fmt.Fprintln(os.Stderr, "VCS broker migration: a previous session's broker remains until that session exits; it is no longer reachable from the guest.")
	}

	ownerID, err := m.newID()
	if err != nil {
		return fmt.Errorf("creating VCS broker owner identity: %w", err)
	}
	generationID, err := m.newID()
	if err != nil {
		return fmt.Errorf("creating VCS broker generation identity: %w", err)
	}
	serviceConfig := newVCSBrokerServiceConfig(m.stateDir, store.StatePath, ownerID, generationID, 1, profile, backendName, guestHome, p.Workspace, specs, configHash, m.buildID)
	state, err = m.runtime.Start(serviceConfig)
	if err != nil {
		return err
	}
	recorded, err := locked.Load()
	if err != nil {
		_ = m.runtime.Stop(backend, profile, state)
		return err
	}
	if !vcsBrokerOwnershipStateMatches(recorded, state) || state.OwnerID != ownerID || state.ConfigHash != configHash || state.BuildID != m.buildID {
		_ = m.runtime.Stop(backend, profile, state)
		return fmt.Errorf("VCS broker service published mismatched ownership state")
	}
	return nil
}

// vcsBrokerOwnershipStateMatches reports whether on-disk state identifies the
// same broker generation as the value Start returned. WriteServiceState stamps
// Version onto a local copy, so the daemon's ready state (Version 0) is not
// byte-equal to the record it published.
func vcsBrokerOwnershipStateMatches(recorded, published vcsbroker.ServiceState) bool {
	return recorded.OwnerID == published.OwnerID &&
		recorded.GenerationID == published.GenerationID &&
		recorded.BrokerPID == published.BrokerPID &&
		recorded.BrokerIdentity == published.BrokerIdentity &&
		recorded.TunnelPID == published.TunnelPID &&
		recorded.TunnelIdentity == published.TunnelIdentity &&
		recorded.HostPort == published.HostPort &&
		recorded.Token == published.Token &&
		recorded.ConfigHash == published.ConfigHash &&
		recorded.BuildID == published.BuildID
}

func runtimeVCSBrokerProcessAlive(runtime vcsBrokerRuntime, state vcsbroker.ServiceState) bool {
	return runtimeVCSBrokerProcessObservation(runtime, state).State == processidentity.Ours
}

func runtimeVCSBrokerProcessObservation(runtime vcsBrokerRuntime, state vcsbroker.ServiceState) processidentity.Observation {
	if observer, ok := runtime.(vcsBrokerProcessObserver); ok {
		return observer.ProcessObservation(state)
	}
	inspector, ok := runtime.(vcsBrokerProcessInspector)
	if ok && inspector.ProcessAlive(state) {
		return processidentity.Observation{State: processidentity.Ours}
	}
	return processidentity.Observation{State: processidentity.Dead}
}

func vcsBrokerHealthProcessObservation(health vcsBrokerHealth) processidentity.Observation {
	if health.Process.State != processidentity.Unverifiable || health.Process.Err != nil {
		return health.Process
	}
	if health.ProcessAlive {
		return processidentity.Observation{State: processidentity.Ours}
	}
	return processidentity.Observation{State: processidentity.Dead}

}

func unverifiableVCSBrokerProcessError(pid int, cause error) error {
	return unverifiableVCSBrokerOwnerError("VCS broker", pid, cause)
}

func unverifiableVCSBrokerTunnelError(pid int, cause error) error {
	return unverifiableVCSBrokerOwnerError("VCS broker tunnel", pid, cause)
}

func unverifiableVCSBrokerOwnerError(kind string, pid int, cause error) error {
	if cause == nil {
		cause = errors.New("kernel process identity is unavailable")
	}
	command, err := vcsBrokerProcessCommandFn(pid)
	if err != nil || command == "" {
		command = "<command unavailable>"
	}
	return fmt.Errorf("%s PID %d is alive but its ownership cannot be verified: %v; command: %q; no process or service state was changed; if this is stale, run 'kill %d', wait for it to exit, then retry 'cloister repair'", kind, pid, cause, command, pid)
}

func (m *vcsBrokerManager) adoptReadyVCSBrokerGeneration(backend vm.Backend, profile, statePath string, locked *vcsbroker.StateLock) (vcsbroker.ServiceState, error) {
	current, err := locked.Load()
	if err != nil {
		return vcsbroker.ServiceState{}, err
	}
	matches, err := filepath.Glob(filepath.Join(m.stateDir, "vcs-broker-generation-*.ready.json"))
	if err != nil {
		return vcsbroker.ServiceState{}, err
	}
	var candidates []vcsbroker.ServiceState
	var adopted vcsbroker.ServiceState
	for _, readyPath := range matches {
		generationID := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(readyPath), "vcs-broker-generation-"), ".ready.json")
		if !validVCSBrokerGenerationID(generationID) {
			continue
		}
		data, readErr := os.ReadFile(readyPath)
		if readErr != nil {
			continue
		}
		var ready vcsBrokerReady
		if json.Unmarshal(data, &ready) != nil {
			continue
		}
		state := ready.State
		recordedStatePath := state.StatePath
		setVCSBrokerStatePaths(m.stateDir, statePath, &state)
		configPath := filepath.Join(m.stateDir, "vcs-broker-generation-"+generationID+".json")
		var generationConfig vcsBrokerServiceConfig
		configData, configErr := os.ReadFile(configPath)
		if configErr != nil || json.Unmarshal(configData, &generationConfig) != nil || generationConfig.StatePath != statePath ||
			generationConfig.GenerationID != generationID || ready.GenerationID != generationID || state.GenerationID != generationID {
			if state.BrokerPID > 0 && state.BrokerIdentity.StartTime != "" {
				observation := processidentity.Observe(state.BrokerPID, state.BrokerIdentity)
				if observation.State == processidentity.Ours || observation.State == processidentity.Unverifiable {
					return vcsbroker.ServiceState{}, fmt.Errorf("generation %q has malformed metadata for live PID %d; refusing cleanup", generationID, state.BrokerPID)
				}
			}
			removeVCSBrokerGenerationFiles(vcsBrokerServiceConfig{GenerationID: generationID, StatePath: statePath})
			continue
		}
		observation := runtimeVCSBrokerProcessObservation(m.runtime, state)
		if observation.State == processidentity.Unverifiable {
			return vcsbroker.ServiceState{}, unverifiableVCSBrokerProcessError(state.BrokerPID, observation.Err)
		}
		if observation.State == processidentity.Ours && !ready.Ready {
			return vcsbroker.ServiceState{}, fmt.Errorf("VCS broker generation %q is still starting; refusing to launch a competitor", generationID)
		}
		if ready.Ready && validVCSBrokerState(state) && recordedStatePath == statePath && observation.State == processidentity.Ours {
			candidates = append(candidates, state)
			if adopted.OwnerID == "" || state.GenerationOrder > adopted.GenerationOrder {
				adopted = state
			} else if state.GenerationOrder == adopted.GenerationOrder && state.GenerationID != adopted.GenerationID {
				return vcsbroker.ServiceState{}, fmt.Errorf("multiple ready VCS broker generations have order %d; refusing ambiguous adoption", state.GenerationOrder)
			}
			continue
		}
		if observation.State == processidentity.Ours {
			return vcsbroker.ServiceState{}, fmt.Errorf("live VCS broker generation %q has invalid readiness metadata; refusing to remove it", generationID)
		}
		generationConfig.StatePath = statePath
		removeVCSBrokerGenerationFiles(generationConfig)
	}
	if adopted.OwnerID == "" {
		return current, nil
	}
	if current.OwnerID != adopted.OwnerID || current.GenerationID != adopted.GenerationID {
		adopted.EnsureHelperPID = current.EnsureHelperPID
		adopted.EnsureHelperIdentity = current.EnsureHelperIdentity
		adopted.EnsureError = current.EnsureError
		adopted.EnsureCompletedAt = current.EnsureCompletedAt
		if err := locked.Save(adopted); err != nil {
			return vcsbroker.ServiceState{}, fmt.Errorf("adopting ready VCS broker generation: %w", err)
		}
	}
	for _, candidate := range candidates {
		if candidate.GenerationID == adopted.GenerationID {
			continue
		}
		candidate.Phase = "retiring"
		if err := m.runtime.RequestShutdown(candidate); err != nil {
			return adopted, fmt.Errorf("adopted generation %q but could not retire older generation %q: %w", adopted.GenerationID, candidate.GenerationID, err)
		}
	}
	return adopted, nil
}

func describeVCSBrokerActivity(state vcsbroker.ServiceState) string {
	data, err := os.ReadFile(state.ActivityPath)
	if err != nil {
		return ""
	}
	var activity vcsBrokerActivity
	if json.Unmarshal(data, &activity) != nil || activity.OwnerID != state.OwnerID ||
		activity.GenerationID != state.GenerationID || len(activity.Commands) == 0 {
		return ""
	}
	return "; draining " + describeVCSBrokerCommands(activity.Commands)
}

func readVCSBrokerTransitionWarning(state vcsbroker.ServiceState) error {
	if state.EnsureError != "" {
		return fmt.Errorf("background ensure failed at %s: %s", state.EnsureCompletedAt.Format(time.RFC3339), state.EnsureError)
	}
	data, err := os.ReadFile(state.TransitionPath)
	if err != nil {
		return nil
	}
	var status vcsBrokerTransitionStatus
	if json.Unmarshal(data, &status) != nil || status.OwnerID != state.OwnerID || status.GenerationID != state.GenerationID {
		return nil
	}
	if status.Error != "" {
		if status.RetryAt.IsZero() {
			if len(status.Commands) > 0 {
				return fmt.Errorf("VCS broker transition %s after attempt %d: %s; interrupted %s", status.State, status.Attempt, status.Error, describeVCSBrokerCommands(status.Commands))
			}
			return fmt.Errorf("VCS broker transition %s after attempt %d: %s; run 'cloister repair' to retry", status.State, status.Attempt, status.Error)
		}
		return fmt.Errorf("VCS broker transition %s after attempt %d: %s; it will retry at %s", status.State, status.Attempt, status.Error, status.RetryAt.Format(time.RFC3339))
	}
	return fmt.Errorf("VCS broker transition %s (attempt %d); current mappings remain available", status.State, status.Attempt)
}

func (m *vcsBrokerManager) stop(backend vm.Backend, profile string) error {
	store := vcsbroker.NewStateStore(m.stateDir, profile, m.lockWait)
	locked, err := store.Lock(context.Background())
	if err != nil {
		return err
	}
	state, err := m.adoptReadyVCSBrokerGeneration(backend, profile, store.StatePath, locked)
	if err != nil {
		_ = locked.Close()
		return err
	}
	if state.OwnerID == "" {
		err := locked.Remove()
		_ = locked.Close()
		return err
	}
	setVCSBrokerStatePaths(m.stateDir, store.StatePath, &state)
	state.Phase = "stopping"
	if err := locked.Save(state); err != nil {
		_ = locked.Close()
		return err
	}
	if err := locked.Close(); err != nil {
		return err
	}
	stopErr := m.runtime.Stop(backend, profile, state)
	locked, err = store.Lock(context.Background())
	if err != nil {
		return err
	}
	defer locked.Close()
	current, err := locked.Load()
	if err != nil {
		return err
	}
	var drainErr *vcsBrokerDrainTimeoutError
	stopCompleted := stopErr == nil || errors.As(stopErr, &drainErr)
	if stopCompleted && current.OwnerID == state.OwnerID && current.GenerationID == state.GenerationID {
		if err := locked.Remove(); err != nil {
			return err
		}
	}
	return stopErr
}

func newVCSBrokerServiceConfig(stateDir, statePath, ownerID, generationID string, generationOrder uint64, profile, backendName, guestHome string, workspace config.WorkspaceConfig, specs []broker.SessionSpec, configHash, buildID string) vcsBrokerServiceConfig {
	base := filepath.Join(stateDir, "vcs-broker-generation-"+generationID)
	return vcsBrokerServiceConfig{
		OwnerID: ownerID, GenerationID: generationID, GenerationOrder: generationOrder, Profile: profile, Backend: backendName,
		GuestHome: guestHome, Specs: specs, Workspace: workspace, ConfigHash: configHash, BuildID: buildID,
		StatePath: statePath, ReadyPath: base + ".ready.json", RepairPath: base + ".repair.json",
		DrainPath: base + ".drain.json", ActivityPath: base + ".activity.json",
		TransitionPath: base + ".transition.json", SpoolDir: base + ".spools",
		RequestPath: base + ".request.json", ConfigPath: base + ".json",
		LogPath: base + ".log", DrainWait: vcsBrokerDrainWait,
	}
}

func serviceConfigForExisting(state vcsbroker.ServiceState, profile, backendName, guestHome string, workspace config.WorkspaceConfig, specs []broker.SessionSpec, configHash, buildID string) vcsBrokerServiceConfig {
	return vcsBrokerServiceConfig{
		OwnerID: state.OwnerID, GenerationID: state.GenerationID, GenerationOrder: state.GenerationOrder, Profile: profile, Backend: backendName,
		GuestHome: guestHome, Specs: specs, Workspace: workspace, ConfigHash: configHash, BuildID: buildID,
		StatePath: state.StatePath, ReadyPath: state.ReadyPath, RepairPath: state.RepairPath,
		DrainPath: state.DrainPath, ActivityPath: state.ActivityPath, TransitionPath: state.TransitionPath,
		RequestPath: state.RequestPath, SpoolDir: state.SpoolDir, ConfigPath: state.ConfigPath,
		LogPath: state.LogPath, DrainWait: vcsBrokerDrainWait,
	}
}

func replacementVCSBrokerServiceConfig(stateDir string, current vcsBrokerServiceConfig, generationID string) vcsBrokerServiceConfig {
	return newVCSBrokerServiceConfig(
		stateDir, current.StatePath, current.OwnerID, generationID, current.GenerationOrder+1, current.Profile,
		current.Backend, current.GuestHome, current.Workspace, current.Specs,
		current.ConfigHash, current.BuildID,
	)
}

func validVCSBrokerState(state vcsbroker.ServiceState) bool {
	return state.OwnerID != "" && validVCSBrokerGenerationID(state.GenerationID) && state.BrokerPID > 0 && state.BrokerIdentity.StartTime != "" &&
		state.TunnelPID > 0 && state.TunnelIdentity.StartTime != "" &&
		state.HostPort > 0 && state.GuestPort == vcsBrokerGuestPort && state.Token != "" &&
		state.ConfigHash != "" && state.BuildID != "" && state.TunnelTarget != "" && state.StatePath != "" && state.ConfigPath != "" &&
		state.ReadyPath != "" && state.RepairPath != "" && state.DrainPath != "" &&
		state.ActivityPath != "" && state.TransitionPath != "" && state.RequestPath != "" && state.SpoolDir != "" && state.LogPath != ""
}

func validVCSBrokerGenerationID(generationID string) bool {
	return len(generationID) <= 128 && vcsBrokerGenerationIDPattern.MatchString(generationID)
}

func setVCSBrokerStatePaths(stateDir, statePath string, state *vcsbroker.ServiceState) {
	if state == nil || !validVCSBrokerGenerationID(state.GenerationID) {
		return
	}
	base := filepath.Join(stateDir, "vcs-broker-generation-"+state.GenerationID)
	state.StatePath = statePath
	state.ConfigPath = base + ".json"
	state.ReadyPath = base + ".ready.json"
	state.RepairPath = base + ".repair.json"
	state.DrainPath = base + ".drain.json"
	state.ActivityPath = base + ".activity.json"
	state.TransitionPath = base + ".transition.json"
	state.RequestPath = base + ".request.json"
	state.SpoolDir = base + ".spools"
	state.LogPath = base + ".log"
}

func resolveVCSBrokerGuestHome(backend vm.Backend, profile string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), vcsbroker.GuestControlTimeout)
	defer cancel()
	out, err := vm.SSHCaptureContext(ctx, backend, profile, `printf '__CLH[%s]CLH__' "$HOME"`)
	if err != nil {
		return "", fmt.Errorf("resolving guest home for VCS broker: %w", err)
	}
	start := strings.Index(out, "__CLH[")
	end := strings.Index(out, "]CLH__")
	if start < 0 || end < 0 || end < start {
		return "", fmt.Errorf("resolving guest home for VCS broker: unexpected output %q", out)
	}
	home := strings.TrimSpace(out[start+len("__CLH[") : end])
	if home == "" {
		return "", fmt.Errorf("resolving guest home for VCS broker: empty home")
	}
	return home, nil
}

func vcsBrokerConfigHash(guestHome string, workspace config.WorkspaceConfig, specs []broker.SessionSpec) (string, error) {
	data, err := json.Marshal(struct {
		GuestHome string
		Workspace config.WorkspaceConfig
		Specs     []broker.SessionSpec
	}{guestHome, workspace, specs})
	if err != nil {
		return "", fmt.Errorf("hashing VCS broker configuration: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func currentVCSBrokerBuildID() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	identity := Version
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" || setting.Key == "vcs.modified" {
				identity += "|" + setting.Key + "=" + setting.Value
			}
		}
	}
	return identity
}

type realVCSBrokerRuntime struct{}

func (realVCSBrokerRuntime) Inspect(backend vm.Backend, profile string, state vcsbroker.ServiceState) vcsBrokerHealth {
	observation := vcsBrokerProcessObservation(state.BrokerPID, state.OwnerID, state.GenerationID, state.BrokerIdentity)
	if observation.State != processidentity.Ours {
		return vcsBrokerHealth{Process: observation}
	}
	hostStatus := vcsbroker.ProbeHost(state.HostPort, state.Token)
	if hostStatus != vcsbroker.HostProbeHealthy {
		return vcsBrokerHealth{Host: hostStatus, ProcessAlive: true, Process: observation}
	}
	claim := tunnel.ReverseForwardOwner{
		OwnerID: state.GenerationID, PID: state.TunnelPID, ProcessIdentity: state.TunnelIdentity, HostPort: state.HostPort,
		GuestPort: state.GuestPort, Target: state.TunnelTarget,
	}
	tunnelObservation := tunnel.OwnedReverseForwardProcessState(profile, "vcs-broker", claim)
	return vcsBrokerHealth{
		Host: vcsbroker.HostProbeHealthy, ProcessAlive: true, Process: observation,
		TunnelProcess: tunnelObservation,
		Tunnel: tunnel.OwnedReverseForwardHealthy(profile, "vcs-broker", claim) &&
			probeVCSBrokerGuestWithRetryFn(backend, profile, state.GuestPort, state.Token, state.GenerationID),
	}
}

func (realVCSBrokerRuntime) ProcessAlive(state vcsbroker.ServiceState) bool {
	return vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID, state.GenerationID, state.BrokerIdentity)
}

func (realVCSBrokerRuntime) ProcessObservation(state vcsbroker.ServiceState) processidentity.Observation {
	return vcsBrokerProcessObservation(state.BrokerPID, state.OwnerID, state.GenerationID, state.BrokerIdentity)
}

func (realVCSBrokerRuntime) Start(cfg vcsBrokerServiceConfig) (vcsbroker.ServiceState, error) {
	_ = os.Remove(cfg.ReadyPath)
	if err := writePrivateJSON(cfg.ConfigPath, cfg); err != nil {
		return vcsbroker.ServiceState{}, fmt.Errorf("writing VCS broker service config: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		_ = os.Remove(cfg.ConfigPath)
		return vcsbroker.ServiceState{}, fmt.Errorf("locating cloister executable: %w", err)
	}
	logFile, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = os.Remove(cfg.ConfigPath)
		return vcsbroker.ServiceState{}, fmt.Errorf("opening VCS broker log: %w", err)
	}
	command := exec.Command(executable, "vcs-broker", "serve", cfg.Profile,
		"--config", cfg.ConfigPath, "--owner", cfg.OwnerID, "--generation", cfg.GenerationID)
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		_ = os.Remove(cfg.ConfigPath)
		_ = os.Remove(cfg.LogPath)
		return vcsbroker.ServiceState{}, fmt.Errorf("starting VCS broker service: %w", err)
	}
	pid := command.Process.Pid
	identity, identityErr := processidentity.Read(pid)
	if identityErr != nil {
		_ = logFile.Close()
		_ = command.Process.Release()
		return vcsbroker.ServiceState{}, fmt.Errorf("capturing VCS broker process identity: %w", identityErr)
	}
	_ = command.Process.Release()
	_ = logFile.Close()
	failed := true
	defer func() {
		if failed {
			cleanupFailedVCSBrokerStart(cfg, pid, identity)
		}
	}()

	deadline := time.Now().Add(vcsBrokerStartupWait)
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(cfg.ReadyPath); readErr == nil {
			var ready vcsBrokerReady
			if err := json.Unmarshal(data, &ready); err != nil {
				return vcsbroker.ServiceState{}, fmt.Errorf("decoding VCS broker readiness: %w", err)
			}
			if ready.Error != "" {
				return vcsbroker.ServiceState{}, fmt.Errorf("starting VCS broker service: %s", ready.Error)
			}
			if !ready.Ready {
				time.Sleep(25 * time.Millisecond)
				continue
			}
			state := ready.State
			if ready.OwnerID != cfg.OwnerID || ready.GenerationID != cfg.GenerationID || ready.BrokerPID != pid ||
				state.OwnerID != cfg.OwnerID || state.GenerationID != cfg.GenerationID || state.BrokerPID != pid || state.BrokerIdentity != identity {
				return vcsbroker.ServiceState{}, fmt.Errorf("VCS broker readiness did not match the launched generation")
			}
			if current, currentErr := vcsbroker.ReadServiceState(cfg.StatePath); currentErr == nil {
				state.EnsureHelperPID = current.EnsureHelperPID
				state.EnsureHelperIdentity = current.EnsureHelperIdentity
				state.EnsureError = current.EnsureError
				state.EnsureCompletedAt = current.EnsureCompletedAt
			}
			if err := vcsbroker.WriteServiceState(cfg.StatePath, state); err != nil {
				return vcsbroker.ServiceState{}, fmt.Errorf("publishing VCS broker generation: %w", err)
			}
			failed = false
			return state, nil
		}
		if !vcsBrokerProcessAlive(pid) {
			logData, _ := os.ReadFile(cfg.LogPath)
			return vcsbroker.ServiceState{}, fmt.Errorf("VCS broker service exited during startup: %s", strings.TrimSpace(string(logData)))
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = stopVCSBrokerProcess(pid, cfg.OwnerID, cfg.GenerationID, identity)
	return vcsbroker.ServiceState{}, fmt.Errorf("timed out after %s starting VCS broker service", vcsBrokerStartupWait)
}

func cleanupFailedVCSBrokerStart(cfg vcsBrokerServiceConfig, pid int, identity processidentity.Identity) {
	if vcsBrokerProcessMatches(pid, cfg.OwnerID, cfg.GenerationID, identity) {
		_ = processidentity.Signal(pid, identity, syscall.SIGTERM)
	}
	var launched vcsbroker.ServiceState
	if data, err := os.ReadFile(cfg.ReadyPath); err == nil {
		var ready vcsBrokerReady
		if json.Unmarshal(data, &ready) == nil && ready.GenerationID == cfg.GenerationID && ready.BrokerPID == pid {
			launched = ready.State
		}
	}
	if launched.GenerationID == cfg.GenerationID && launched.BrokerPID == pid {
		if backend, resolveErr := resolveBackend(cfg.Backend); resolveErr == nil {
			vcsbroker.RemoveGuestConfig(backend, cfg.Profile, cfg.GenerationID)
		}
		claim := tunnel.ReverseForwardOwner{
			OwnerID: launched.GenerationID, PID: launched.TunnelPID, ProcessIdentity: launched.TunnelIdentity, HostPort: launched.HostPort,
			GuestPort: launched.GuestPort, Target: launched.TunnelTarget,
		}
		tunnel.StopOwnedReverseForward(cfg.Profile, "vcs-broker", claim)
	}
	removeVCSBrokerGenerationFiles(cfg)
}

func (realVCSBrokerRuntime) RequestTunnelRepair(state vcsbroker.ServiceState) error {
	if !vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID, state.GenerationID, state.BrokerIdentity) {
		return fmt.Errorf("VCS broker daemon is not running")
	}
	_ = os.Remove(state.RepairPath)
	if err := processidentity.Signal(state.BrokerPID, state.BrokerIdentity, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("requesting tunnel repair: %w", err)
	}
	return nil
}

func (realVCSBrokerRuntime) RequestRestart(cfg vcsBrokerServiceConfig, state vcsbroker.ServiceState) error {
	if !vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID, state.GenerationID, state.BrokerIdentity) {
		return fmt.Errorf("VCS broker daemon is not running")
	}
	if err := writePrivateJSON(state.RequestPath, cfg); err != nil {
		return fmt.Errorf("writing deferred VCS broker configuration: %w", err)
	}
	_ = writePrivateJSON(state.TransitionPath, vcsBrokerTransitionStatus{OwnerID: state.OwnerID, GenerationID: state.GenerationID, Attempt: 0, State: "requested"})
	if err := processidentity.Signal(state.BrokerPID, state.BrokerIdentity, syscall.SIGUSR2); err != nil {
		return fmt.Errorf("requesting deferred VCS broker restart: %w", err)
	}
	return nil
}

func (realVCSBrokerRuntime) RequestShutdown(state vcsbroker.ServiceState) error {
	if !vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID, state.GenerationID, state.BrokerIdentity) {
		return fmt.Errorf("VCS broker daemon is not running")
	}
	if err := processidentity.Signal(state.BrokerPID, state.BrokerIdentity, syscall.SIGTERM); err != nil {
		return fmt.Errorf("requesting VCS broker shutdown: %w", err)
	}
	return nil
}

func (realVCSBrokerRuntime) Stop(backend vm.Backend, profile string, state vcsbroker.ServiceState) error {
	observation := vcsBrokerProcessObservation(state.BrokerPID, state.OwnerID, state.GenerationID, state.BrokerIdentity)
	if observation.State == processidentity.Unverifiable {
		return unverifiableVCSBrokerProcessError(state.BrokerPID, observation.Err)
	}
	claim := tunnel.ReverseForwardOwner{
		OwnerID: state.GenerationID, PID: state.TunnelPID, ProcessIdentity: state.TunnelIdentity, HostPort: state.HostPort,
		GuestPort: state.GuestPort, Target: state.TunnelTarget,
	}
	tunnelObservation := tunnel.OwnedReverseForwardProcessState(profile, "vcs-broker", claim)
	if tunnelObservation.State == processidentity.Unverifiable {
		return unverifiableVCSBrokerTunnelError(state.TunnelPID, tunnelObservation.Err)
	}
	_ = os.Remove(state.DrainPath)
	pid := state.BrokerPID
	if observation.State == processidentity.Ours {
		if err := processidentity.Signal(pid, state.BrokerIdentity, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("signaling VCS broker service: %w", err)
		}
		deadline := time.Now().Add(vcsBrokerShutdownWait)
		startedWaiting := time.Now()
		nextProgress := startedWaiting.Add(vcsBrokerProgressEvery)
		for processidentity.Matches(pid, state.BrokerIdentity) && time.Now().Before(deadline) {
			if !time.Now().Before(nextProgress) {
				if status, statusErr := vcsbroker.ReadHostStatus(state.HostPort, state.Token); statusErr == nil && len(status.Commands) > 0 {
					writeVCSBrokerStopProgress(os.Stderr, time.Since(startedWaiting), status.Commands)
				}
				nextProgress = time.Now().Add(vcsBrokerProgressEvery)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if processidentity.Matches(pid, state.BrokerIdentity) {
			if err := stopVCSBrokerProcess(pid, state.OwnerID, state.GenerationID, state.BrokerIdentity); err != nil {
				return err
			}
		}
	}
	vcsbroker.RemoveGuestConfig(backend, profile, state.GenerationID)
	tunnel.StopOwnedReverseForward(profile, "vcs-broker", claim)
	var drainErr error
	if data, err := os.ReadFile(state.DrainPath); err == nil {
		var report vcsBrokerDrainReport
		if json.Unmarshal(data, &report) == nil && report.OwnerID == state.OwnerID &&
			report.GenerationID == state.GenerationID && len(report.Commands) > 0 {
			drainErr = formatVCSBrokerDrainError(report.Commands)
		}
	}
	removeVCSBrokerGenerationStateFiles(state)
	return drainErr
}

func writeVCSBrokerStopProgress(out io.Writer, waited time.Duration, commands []vcsbroker.ActiveCommand) {
	fmt.Fprintf(out, "Waiting %s for VCS broker commands to finish: %s\n",
		waited.Round(time.Second), describeVCSBrokerCommands(commands))
}

func (realVCSBrokerRuntime) ForceStop(backend vm.Backend, profile string, state vcsbroker.ServiceState) error {
	observation := vcsBrokerProcessObservation(state.BrokerPID, state.OwnerID, state.GenerationID, state.BrokerIdentity)
	if observation.State == processidentity.Unverifiable {
		return unverifiableVCSBrokerProcessError(state.BrokerPID, observation.Err)
	}
	claim := tunnel.ReverseForwardOwner{
		OwnerID: state.GenerationID, PID: state.TunnelPID, ProcessIdentity: state.TunnelIdentity, HostPort: state.HostPort,
		GuestPort: state.GuestPort, Target: state.TunnelTarget,
	}
	tunnelObservation := tunnel.OwnedReverseForwardProcessState(profile, "vcs-broker", claim)
	if tunnelObservation.State == processidentity.Unverifiable {
		return unverifiableVCSBrokerTunnelError(state.TunnelPID, tunnelObservation.Err)
	}
	pid := state.BrokerPID
	if observation.State == processidentity.Ours {
		if err := stopVCSBrokerProcess(pid, state.OwnerID, state.GenerationID, state.BrokerIdentity); err != nil {
			return err
		}
	}
	vcsbroker.RemoveGuestConfig(backend, profile, state.GenerationID)
	tunnel.StopOwnedReverseForward(profile, "vcs-broker", claim)
	removeVCSBrokerGenerationStateFiles(state)
	return nil
}

func formatVCSBrokerDrainError(commands []vcsbroker.ActiveCommand) error {
	return &vcsBrokerDrainTimeoutError{Commands: append([]vcsbroker.ActiveCommand(nil), commands...)}
}

func vcsBrokerProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

func vcsBrokerProcessMatches(pid int, ownerID, generationID string, identity processidentity.Identity) bool {
	return vcsBrokerProcessObservation(pid, ownerID, generationID, identity).State == processidentity.Ours
}

func vcsBrokerProcessObservation(pid int, ownerID, generationID string, identity processidentity.Identity) processidentity.Observation {
	if ownerID == "" || generationID == "" {
		return processidentity.Observation{State: processidentity.Unverifiable, Err: errors.New("service owner or generation identity is missing")}
	}
	return observeVCSBrokerProcessFn(pid, identity)
}

func adjacentCommandFields(fields []string, first, second string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == first && fields[i+1] == second {
			return true
		}
	}
	return false
}

func commandFlagEquals(fields []string, flag, value string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == flag && fields[i+1] == value {
			return true
		}
	}
	return false
}

func stopVCSBrokerProcess(pid int, ownerID, generationID string, identity processidentity.Identity) error {
	if !vcsBrokerProcessMatches(pid, ownerID, generationID, identity) {
		return fmt.Errorf("refusing to kill VCS broker PID %d without exact generation identity", pid)
	}
	// The standalone daemon starts a new session and its VCS children inherit
	// that process group. A forced stop targets the group so a child cannot
	// continue mutating a repository after the broker is gone.
	if err := processidentity.SignalProcessGroup(pid, pid, identity, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func newVCSBrokerID() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vcs-broker-json-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

var vcsBrokerServeConfig string
var vcsBrokerServeOwner string
var vcsBrokerServeGeneration string

var vcsBrokerCmd = &cobra.Command{
	Use:    "vcs-broker",
	Short:  "Run Cloister's internal per-profile VCS broker service",
	Hidden: true,
	Long: `Internal service command managed by Cloister profile lifecycle operations.
It owns one authenticated host broker and reverse tunnel for a running VM.
Direct duplicate invocation is rejected by tunnel ownership, and invocation
against a stopped VM fails because the authenticated guest path cannot start.`,
}

var vcsBrokerServeCmd = &cobra.Command{
	Use:    "serve <profile>",
	Short:  "Serve one internally managed profile VCS broker",
	Hidden: true,
	Long: `Internal daemon started only by Cloister profile lifecycle management.
It owns the authenticated host listener, guest configuration, and reverse
tunnel for one running profile. A direct duplicate is rejected, and a direct
invocation outside its detached process group or against a stopped VM fails.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runVCSBrokerService(args[0], vcsBrokerServeConfig, vcsBrokerServeOwner, vcsBrokerServeGeneration)
	},
}

var vcsBrokerWatchChildCmd = &cobra.Command{
	Use:   "watch-child <process-group> <child-pid> <start-time> <executable>",
	Short: "Watch one internal broker child process",
	Long: `Internal watchdog started by the VCS broker beside each accepted host
command. If the owning daemon disappears, it kills the daemon's private process
group so the command cannot continue mutating a repository without supervision.`,
	Hidden: true,
	Args:   cobra.ExactArgs(4),
	RunE: func(cmd *cobra.Command, args []string) error {
		processGroup, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid VCS broker process group")
		}
		childPID, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid VCS child PID")
		}
		return runVCSBrokerChildWatchdog(processGroup, childPID, processidentity.Identity{StartTime: args[2], Executable: args[3]})
	},
}

var vcsBrokerEnsureCmd = &cobra.Command{
	Use:    "ensure <profile>",
	Short:  "Ensure one profile broker in a detached lifecycle helper",
	Hidden: true,
	Long: `Internal helper launched by interactive and headless entry paths.
It serializes broker inspection and repair for one profile, records its durable
outcome for status reporting, and exits without owning the broker process.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runVCSBrokerEnsure(args[0])
	},
}

func init() {
	vcsBrokerServeCmd.Flags().StringVar(&vcsBrokerServeConfig, "config", "", "internal service configuration")
	vcsBrokerServeCmd.Flags().StringVar(&vcsBrokerServeOwner, "owner", "", "internal owner identity")
	vcsBrokerServeCmd.Flags().StringVar(&vcsBrokerServeGeneration, "generation", "", "internal generation identity")
	vcsBrokerCmd.AddCommand(vcsBrokerServeCmd, vcsBrokerWatchChildCmd, vcsBrokerEnsureCmd)
	rootCmd.AddCommand(vcsBrokerCmd)
}

func runVCSBrokerChildWatchdog(processGroup, childPID int, childIdentity processidentity.Identity) error {
	if processGroup <= 1 || childPID <= 1 || syscall.Getpgrp() != processGroup {
		return fmt.Errorf("VCS child watchdog is not in the broker process group")
	}
	childGroup, err := syscall.Getpgid(childPID)
	if err != nil || childGroup != processGroup {
		return fmt.Errorf("VCS child is not in the broker process group")
	}
	parentPipe := os.NewFile(3, "vcs-broker-parent")
	if parentPipe == nil {
		return fmt.Errorf("VCS child watchdog parent pipe is unavailable")
	}
	defer parentPipe.Close()
	if _, err := io.Copy(io.Discard, parentPipe); err != nil {
		return fmt.Errorf("watching VCS broker parent: %w", err)
	}
	if !processidentity.Matches(childPID, childIdentity) {
		return fmt.Errorf("VCS child process identity changed")
	}
	childGroup, err = syscall.Getpgid(childPID)
	if err != nil || childGroup != processGroup {
		return fmt.Errorf("VCS child left the broker process group")
	}
	// EOF means the broker disappeared without stopping this watcher. Kill the
	// private process group so the host command cannot outlive its authority.
	// SIGSTOP deliberately does not trigger this path: the broker still owns
	// the pipe, and letting an accepted repository operation reach its barrier
	// is safer than asynchronously killing it midway through mutation.
	_ = processidentity.SignalProcessGroup(processGroup, childPID, childIdentity, syscall.SIGKILL)
	return nil
}

func newVCSBrokerHostRunner() (vcsbroker.HostCommandRunner, error) {
	if syscall.Getpgrp() != os.Getpid() {
		return nil, fmt.Errorf("VCS broker service must be started by Cloister lifecycle management")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating VCS child watchdog executable: %w", err)
	}
	return vcsbroker.NewSupervisedRunner(executable, syscall.Getpgrp()), nil
}

func runVCSBrokerService(profile, configPath, ownerID, generationID string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg vcsBrokerServiceConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if profile == "" || cfg.Profile != profile || ownerID == "" || cfg.OwnerID != ownerID ||
		generationID == "" || cfg.GenerationID != generationID || cfg.ConfigPath != configPath {
		return fmt.Errorf("VCS broker service identity does not match its configuration")
	}
	if err := validateVCSBrokerServicePaths(cfg); err != nil {
		return err
	}
	service, err := startVCSBrokerService(cfg)
	if err != nil {
		_ = writePrivateJSON(cfg.ReadyPath, vcsBrokerReady{Error: err.Error()})
		return err
	}
	if err := writePrivateJSON(cfg.ReadyPath, vcsBrokerReady{
		OwnerID: ownerID, GenerationID: generationID, BrokerPID: os.Getpid(), State: service.state, Ready: true,
	}); err != nil {
		service.shutdown(cfg)
		return err
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	defer signal.Stop(signals)
	ticker := time.NewTicker(vcsBrokerVMCheckEvery)
	defer ticker.Stop()
	maintenance := time.NewTicker(100 * time.Millisecond)
	defer maintenance.Stop()
	return runVCSBrokerServiceLoop(service, cfg, signals, ticker.C, maintenance.C)
}

type runningVCSBrokerService struct {
	state            vcsbroker.ServiceState
	backend          vm.Backend
	server           *vcsbroker.Server
	tunnel           tunnel.ReverseForwardOwner
	activityMu       sync.Mutex
	activityRevision uint64
}

func startVCSBrokerService(cfg vcsBrokerServiceConfig) (*runningVCSBrokerService, error) {
	backend, err := resolveBackend(cfg.Backend)
	if err != nil {
		return nil, err
	}
	hash, err := vcsBrokerConfigHash(cfg.GuestHome, cfg.Workspace, cfg.Specs)
	if err != nil || hash != cfg.ConfigHash {
		return nil, fmt.Errorf("VCS broker service configuration hash mismatch")
	}
	if currentVCSBrokerBuildID() != cfg.BuildID {
		return nil, fmt.Errorf("VCS broker service build identity mismatch")
	}
	if cfg.DrainWait <= 0 {
		return nil, fmt.Errorf("VCS broker service drain bound is required")
	}
	syncBroker, err := newWorkspaceBroker()
	if err != nil {
		return nil, err
	}
	mapper, err := vcsbroker.NewMapper(cfg.GuestHome, cfg.Specs)
	if err != nil {
		return nil, err
	}
	runner, err := newVCSBrokerHostRunnerFn()
	if err != nil {
		return nil, err
	}
	token, err := newVCSBrokerID()
	if err != nil {
		return nil, err
	}
	brokerIdentity, err := processidentity.Read(os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("capturing VCS broker process identity: %w", err)
	}
	server, err := vcsbroker.StartServerWithSpoolDir(vcsbroker.NewProxy(syncBroker, mapper, runner), token, cfg.SpoolDir)
	if err != nil {
		return nil, err
	}
	claim, err := startVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", cfg.GenerationID, server.Port(), vcsBrokerGuestPort, backend.SSHConfig(cfg.Profile))
	if err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("starting VCS broker tunnel: %w", err)
	}
	state := vcsbroker.ServiceState{
		OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, GenerationOrder: cfg.GenerationOrder, BrokerPID: os.Getpid(), BrokerIdentity: brokerIdentity,
		TunnelPID: claim.PID, TunnelIdentity: claim.ProcessIdentity,
		HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: token,
		ConfigHash: cfg.ConfigHash, BuildID: cfg.BuildID, TunnelTarget: claim.Target,
		StatePath: cfg.StatePath, ConfigPath: cfg.ConfigPath, ReadyPath: cfg.ReadyPath,
		RepairPath: cfg.RepairPath, DrainPath: cfg.DrainPath, ActivityPath: cfg.ActivityPath,
		TransitionPath: cfg.TransitionPath, RequestPath: cfg.RequestPath, SpoolDir: cfg.SpoolDir, LogPath: cfg.LogPath,
	}
	service := &runningVCSBrokerService{state: state, backend: backend, server: server, tunnel: claim}
	server.SetStatusObserver(service.publishActivity)
	// Publish generation-local cleanup identity before the first bounded guest
	// operation. The profile state pointer is not switched until final health.
	if err := writePrivateJSON(cfg.ReadyPath, vcsBrokerReady{
		OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, BrokerPID: os.Getpid(), State: state,
	}); err != nil {
		stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", claim)
		_ = server.Close()
		return nil, err
	}
	if err := deployVCSBrokerGuestFn(backend, cfg.Profile, vcsBrokerGuestPort, token, cfg.GenerationID); err != nil {
		stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", claim)
		_ = server.Close()
		return nil, err
	}
	deadline := time.Now().Add(3 * time.Second)
	for !probeVCSBrokerGuestFn(backend, cfg.Profile, vcsBrokerGuestPort, token, cfg.GenerationID) {
		if time.Now().After(deadline) {
			vcsbroker.RemoveGuestConfig(backend, cfg.Profile, cfg.GenerationID)
			stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", claim)
			_ = server.Close()
			return nil, fmt.Errorf("VCS broker tunnel failed its authenticated guest health check")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return service, nil
}

func (s *runningVCSBrokerService) publishActivity(status vcsbroker.ServerStatus) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if status.Revision < s.activityRevision {
		return
	}
	s.activityRevision = status.Revision
	_ = writePrivateJSON(s.state.ActivityPath, vcsBrokerActivity{
		OwnerID: s.state.OwnerID, GenerationID: s.state.GenerationID, Revision: status.Revision,
		Draining: status.Draining, Commands: status.Commands,
	})
}

type vcsBrokerTransitionResult struct {
	commands []vcsbroker.ActiveCommand
	err      error
}

func runVCSBrokerServiceLoop(service *runningVCSBrokerService, cfg vcsBrokerServiceConfig, signals <-chan os.Signal, ticks, maintenance <-chan time.Time) error {
	misses := 0
	runningTicks := 0
	restartPending := false
	additivePending := false
	transitionAttempt := 0
	var retryAt time.Time
	var transition <-chan vcsBrokerTransitionResult
	startTransition := func() {
		transitionAttempt++
		retryAt = time.Time{}
		_ = writePrivateJSON(cfg.TransitionPath, vcsBrokerTransitionStatus{
			OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, Attempt: transitionAttempt, State: "draining",
		})
		result := make(chan vcsBrokerTransitionResult, 1)
		transition = result
		go drainVCSBrokerTransition(service.server, cfg.DrainWait, result)
	}
	scheduleRetry := func(result vcsBrokerTransitionResult, err error) {
		service.server.Resume()
		if transitionAttempt >= vcsBrokerMaxTransitionAttempts {
			restartPending = false
			additivePending = false
			retryAt = time.Time{}
			_ = writePrivateJSON(cfg.TransitionPath, vcsBrokerTransitionStatus{
				OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, Attempt: transitionAttempt,
				State: "failed", Error: err.Error(), Commands: result.commands,
			})
			fmt.Fprintf(os.Stderr, "VCS broker transition failed after %d attempts: %v; run 'cloister repair %s' to retry\n", transitionAttempt, err, cfg.Profile)
			return
		}
		delay := vcsBrokerTransitionRetryDelay(transitionAttempt)
		retryAt = time.Now().Add(delay)
		_ = writePrivateJSON(cfg.TransitionPath, vcsBrokerTransitionStatus{
			OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, Attempt: transitionAttempt, State: "retry-pending",
			Error: err.Error(), RetryAt: retryAt, Commands: result.commands,
		})
		fmt.Fprintf(os.Stderr, "VCS broker deferred transition attempt %d: %v; retrying in %s\n", transitionAttempt, err, delay)
	}
	scheduleAdditiveRetry := func(err error) {
		transitionAttempt++
		if transitionAttempt >= vcsBrokerMaxTransitionAttempts {
			restartPending = false
			additivePending = false
			retryAt = time.Time{}
			_ = writePrivateJSON(cfg.TransitionPath, vcsBrokerTransitionStatus{
				OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, Attempt: transitionAttempt,
				State: "failed", Error: err.Error(),
			})
			fmt.Fprintf(os.Stderr, "VCS broker additive update failed after %d attempts: %v; run 'cloister repair %s' to retry\n", transitionAttempt, err, cfg.Profile)
			return
		}
		restartPending = true
		additivePending = true
		delay := vcsBrokerTransitionRetryDelay(transitionAttempt)
		retryAt = time.Now().Add(delay)
		_ = writePrivateJSON(cfg.TransitionPath, vcsBrokerTransitionStatus{
			OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, Attempt: transitionAttempt, State: "retry-pending",
			Error: err.Error(), RetryAt: retryAt,
		})
		fmt.Fprintf(os.Stderr, "VCS broker additive update attempt %d: %v; retrying in %s\n", transitionAttempt, err, delay)
	}
	for {
		select {
		case received := <-signals:
			if received == syscall.SIGUSR1 {
				// The reverse forward is transport, not command execution state.
				// Replacing a broken tunnel must not wait behind host work.
				err := service.repairTunnel(cfg)
				result := vcsBrokerRepairReady{OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, TunnelPID: service.state.TunnelPID}
				if err != nil {
					result.Error = err.Error()
				}
				_ = writePrivateJSON(cfg.RepairPath, result)
				continue
			}
			if received == syscall.SIGUSR2 {
				if transition == nil && !restartPending {
					transitionAttempt = 0
					retryAt = time.Time{}
				}
				desired, err := readVCSBrokerServiceConfig(cfg.RequestPath, cfg.OwnerID)
				if err == nil && pureAdditiveVCSBrokerConfig(cfg, desired) {
					if err := service.applyMapperConfig(desired, false); err != nil {
						scheduleAdditiveRetry(err)
					} else {
						cfg = desired
						restartPending = false
						additivePending = false
						transitionAttempt = 0
						_ = os.Remove(cfg.TransitionPath)
					}
					continue
				}
				restartPending = true
				additivePending = false
				if transition == nil {
					startTransition()
				}
				continue
			}
			service.shutdown(cfg)
			if vcsBrokerStateHasPhase(cfg.StatePath, cfg.OwnerID, cfg.GenerationID, "retiring") || !vcsBrokerStateIsGeneration(cfg.StatePath, cfg.OwnerID, cfg.GenerationID) {
				logVCSBrokerDrainReport(cfg.DrainPath, cfg.OwnerID, cfg.GenerationID)
				removeVCSBrokerStateIfOwner(cfg.StatePath, cfg.Profile, cfg.OwnerID, cfg.GenerationID)
				removeVCSBrokerGenerationFiles(cfg)
			}
			return nil
		case result := <-transition:
			transition = nil
			if result.err != nil {
				desired, readErr := readVCSBrokerServiceConfig(cfg.RequestPath, cfg.OwnerID)
				if readErr == nil && desired.RestartDaemon {
					_ = writePrivateJSON(cfg.DrainPath, vcsBrokerDrainReport{OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, Commands: result.commands})
					replaced, replaceErr := service.applyDesiredConfig(cfg, desired)
					if replaceErr == nil && replaced {
						_ = writePrivateJSON(desired.TransitionPath, vcsBrokerTransitionStatus{
							OwnerID: desired.OwnerID, GenerationID: desired.GenerationID, Attempt: transitionAttempt, State: "forced-after-drain-timeout",
							Error: fmt.Sprintf("interrupted after the %s drain bound", cfg.DrainWait), Commands: result.commands,
						})
						fmt.Fprintf(os.Stderr, "VCS broker unhealthy replacement interrupted after the %s drain bound: %s\n", cfg.DrainWait, describeVCSBrokerCommands(result.commands))
						return nil
					}
					if replaceErr != nil {
						scheduleRetry(result, fmt.Errorf("drain expired and replacement failed: %w", replaceErr))
						continue
					}
				}
				scheduleRetry(result, fmt.Errorf("drain exceeded %s while waiting for %s", cfg.DrainWait, describeVCSBrokerCommands(result.commands)))
				continue
			}
			if !restartPending {
				service.server.Resume()
				continue
			}
			desired, err := readVCSBrokerServiceConfig(cfg.RequestPath, cfg.OwnerID)
			if err != nil {
				scheduleRetry(result, fmt.Errorf("reading deferred configuration: %w", err))
				continue
			}
			replaced, err := service.applyDesiredConfig(cfg, desired)
			if err != nil {
				scheduleRetry(result, err)
				continue
			}
			if replaced {
				return nil
			}
			cfg = desired
			restartPending = false
			additivePending = false
			transitionAttempt = 0
			_ = os.Remove(cfg.TransitionPath)
		case <-maintenance:
			if restartPending && transition == nil && !retryAt.IsZero() && !time.Now().Before(retryAt) {
				if additivePending {
					desired, err := readVCSBrokerServiceConfig(cfg.RequestPath, cfg.OwnerID)
					if err != nil || !pureAdditiveVCSBrokerConfig(cfg, desired) {
						additivePending = false
						startTransition()
						continue
					}
					if err := service.applyMapperConfig(desired, false); err != nil {
						scheduleAdditiveRetry(err)
						continue
					}
					cfg = desired
					restartPending = false
					additivePending = false
					transitionAttempt = 0
					retryAt = time.Time{}
					_ = os.Remove(cfg.TransitionPath)
					continue
				}
				startTransition()
			}
		case <-ticks:
			if service.backend.IsRunning(cfg.Profile) {
				misses = 0
				runningTicks++
				if runningTicks >= vcsBrokerGuestVerifyTicks {
					runningTicks = 0
					service.ensureGuestInstallation(cfg)
				}
				continue
			}
			runningTicks = 0
			misses++
			if misses < vcsBrokerVMMisses {
				continue
			}
			service.shutdown(cfg)
			logVCSBrokerDrainReport(cfg.DrainPath, cfg.OwnerID, cfg.GenerationID)
			removeVCSBrokerStateIfOwner(cfg.StatePath, cfg.Profile, cfg.OwnerID, cfg.GenerationID)
			removeVCSBrokerGenerationFiles(cfg)
			return nil
		}
	}
}

func (s *runningVCSBrokerService) ensureGuestInstallation(cfg vcsBrokerServiceConfig) {
	status, repaired, err := ensureVCSBrokerGuestInstallationFn(
		s.backend, cfg.Profile, vcsBrokerGuestPort, s.state.Token, s.state.GenerationID,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "VCS broker guest installation check failed for profile %q: %v\n", cfg.Profile, err)
		return
	}
	if repaired {
		fmt.Fprintf(os.Stderr, "VCS broker guest installation repaired for profile %q: found config=%s shim=%s; replaced the service config and shim without changing the token\n", cfg.Profile, status.Config, status.Shim)
	}
	if probeVCSBrokerGuestWithRetryFn(s.backend, cfg.Profile, vcsBrokerGuestPort, s.state.Token, s.state.GenerationID) {
		// A released session can recreate the integer PID record after upgrade.
		// That stranded ssh holds no guest port, but the record blocks later
		// owned-tunnel repair. Retire it on this ticker while the owned tunnel
		// is still healthy.
		if err := s.retireRecreatedLegacyTunnel(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "VCS broker periodic tunnel repair failed for profile %q: %v\n", cfg.Profile, err)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "VCS broker guest endpoint failed %d authenticated probes for profile %q; repairing its reverse tunnel\n", vcsbroker.HostProbeAttempts, cfg.Profile)
	if err := s.repairTunnel(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "VCS broker periodic tunnel repair failed for profile %q: %v\n", cfg.Profile, err)
		return
	}
	fmt.Fprintf(os.Stderr, "VCS broker periodic tunnel repair completed for profile %q without changing the daemon or token\n", cfg.Profile)
}

func vcsBrokerTransitionRetryDelay(attempt int) time.Duration {
	delay := vcsBrokerTransitionRetryBase
	for i := 1; i < attempt && delay < time.Minute; i++ {
		delay *= 2
	}
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

func pureAdditiveVCSBrokerConfig(current, desired vcsBrokerServiceConfig) bool {
	if desired.RestartDaemon || current.OwnerID != desired.OwnerID || current.GenerationID != desired.GenerationID || current.Profile != desired.Profile ||
		current.Backend != desired.Backend || current.GuestHome != desired.GuestHome ||
		current.BuildID != desired.BuildID || !reflect.DeepEqual(current.Workspace, desired.Workspace) ||
		len(desired.Specs) <= len(current.Specs) {
		return false
	}
	desiredByID := make(map[string]broker.SessionSpec, len(desired.Specs))
	for _, spec := range desired.Specs {
		if _, duplicate := desiredByID[spec.ProjectID]; duplicate {
			return false
		}
		desiredByID[spec.ProjectID] = spec
	}
	for _, spec := range current.Specs {
		candidate, ok := desiredByID[spec.ProjectID]
		if !ok || !reflect.DeepEqual(candidate, spec) {
			return false
		}
	}
	return true
}

func drainVCSBrokerTransition(server *vcsbroker.Server, wait time.Duration, result chan<- vcsBrokerTransitionResult) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	commands, err := server.Pause(ctx)
	result <- vcsBrokerTransitionResult{commands: commands, err: err}
}

func vcsBrokerStateHasPhase(path, ownerID, generationID, phase string) bool {
	state, err := vcsbroker.ReadServiceState(path)
	return err == nil && state.OwnerID == ownerID && state.GenerationID == generationID && state.Phase == phase
}

func vcsBrokerStateIsGeneration(path, ownerID, generationID string) bool {
	state, err := vcsbroker.ReadServiceState(path)
	return err == nil && state.OwnerID == ownerID && state.GenerationID == generationID
}

func logVCSBrokerDrainReport(path, ownerID, generationID string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var report vcsBrokerDrainReport
	if json.Unmarshal(data, &report) == nil && report.OwnerID == ownerID && report.GenerationID == generationID && len(report.Commands) > 0 {
		fmt.Fprintf(os.Stderr, "%v\n", formatVCSBrokerDrainError(report.Commands))
	}
}

func removeVCSBrokerGenerationFiles(cfg vcsBrokerServiceConfig) {
	if !validVCSBrokerGenerationID(cfg.GenerationID) || cfg.StatePath == "" {
		return
	}
	stateDir := filepath.Clean(filepath.Dir(cfg.StatePath))
	base := filepath.Join(stateDir, "vcs-broker-generation-"+cfg.GenerationID)
	for _, path := range []string{base + ".json", base + ".ready.json", base + ".repair.json", base + ".drain.json", base + ".activity.json", base + ".transition.json", base + ".request.json", base + ".log", base + ".spools"} {
		removeConfinedVCSBrokerGenerationPath(stateDir, path)
	}
}

func validateVCSBrokerServicePaths(cfg vcsBrokerServiceConfig) error {
	if !validVCSBrokerGenerationID(cfg.GenerationID) || cfg.Profile == "" || cfg.StatePath == "" {
		return fmt.Errorf("VCS broker generation path identity is invalid")
	}
	stateDir := filepath.Clean(filepath.Dir(cfg.StatePath))
	if vcsbroker.NewStateStore(stateDir, cfg.Profile, vcsBrokerLockWait).StatePath != cfg.StatePath {
		return fmt.Errorf("VCS broker profile state path is not canonical")
	}
	base := filepath.Join(stateDir, "vcs-broker-generation-"+cfg.GenerationID)
	expected := []string{
		base + ".json", base + ".ready.json", base + ".repair.json", base + ".drain.json",
		base + ".activity.json", base + ".transition.json", base + ".request.json",
		base + ".spools", base + ".log",
	}
	actual := []string{
		cfg.ConfigPath, cfg.ReadyPath, cfg.RepairPath, cfg.DrainPath, cfg.ActivityPath,
		cfg.TransitionPath, cfg.RequestPath, cfg.SpoolDir, cfg.LogPath,
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("VCS broker generation paths are not canonical")
	}
	return nil
}

func removeConfinedVCSBrokerGenerationPath(stateDir, path string) {
	resolvedDir, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		return
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return
	}
	relative, err := filepath.Rel(resolvedDir, resolvedPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return
	}
	if info.IsDir() {
		_ = os.RemoveAll(path)
	} else {
		_ = os.Remove(path)
	}
}

func removeVCSBrokerGenerationStateFiles(state vcsbroker.ServiceState) {
	removeVCSBrokerGenerationFiles(vcsBrokerServiceConfig{
		GenerationID: state.GenerationID, StatePath: state.StatePath,
		ConfigPath: state.ConfigPath, ReadyPath: state.ReadyPath, RepairPath: state.RepairPath,
		DrainPath: state.DrainPath, ActivityPath: state.ActivityPath, TransitionPath: state.TransitionPath,
		RequestPath: state.RequestPath, LogPath: state.LogPath, SpoolDir: state.SpoolDir,
	})
}

func readVCSBrokerServiceConfig(path, ownerID string) (vcsBrokerServiceConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return vcsBrokerServiceConfig{}, err
	}
	var cfg vcsBrokerServiceConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return vcsBrokerServiceConfig{}, err
	}
	if cfg.OwnerID != ownerID || cfg.GenerationID == "" || cfg.DrainWait <= 0 {
		return vcsBrokerServiceConfig{}, fmt.Errorf("deferred service identity mismatch")
	}
	return cfg, nil
}

func (s *runningVCSBrokerService) applyDesiredConfig(current, desired vcsBrokerServiceConfig) (bool, error) {
	hash, err := vcsBrokerConfigHash(desired.GuestHome, desired.Workspace, desired.Specs)
	if err != nil || hash != desired.ConfigHash {
		return false, fmt.Errorf("deferred service configuration hash mismatch")
	}
	if desired.BuildID == s.state.BuildID && !desired.RestartDaemon {
		if desired.GenerationID != s.state.GenerationID {
			return false, fmt.Errorf("mapper update changed VCS broker generation identity")
		}
		return false, s.applyMapperConfig(desired, true)
	}
	if desired.GenerationID == "" || desired.GenerationID == s.state.GenerationID {
		return false, fmt.Errorf("replacement requires a distinct VCS broker generation identity")
	}

	store := vcsbroker.NewStateStore(filepath.Dir(current.StatePath), current.Profile, vcsBrokerLockWait)
	if store.StatePath != current.StatePath {
		return false, fmt.Errorf("replacement service state path mismatch")
	}
	locked, err := store.Lock(context.Background())
	if err != nil {
		return false, fmt.Errorf("locking drained VCS broker replacement: %w", err)
	}
	defer locked.Close()
	recorded, err := locked.Load()
	if err != nil || recorded.OwnerID != s.state.OwnerID || recorded.GenerationID != s.state.GenerationID || recorded.BrokerPID != s.state.BrokerPID {
		return false, fmt.Errorf("VCS broker ownership changed before drained replacement")
	}
	desired.RestartDaemon = false
	stopVCSBrokerTunnelFn(current.Profile, "vcs-broker", s.tunnel)
	state, err := startVCSBrokerReplacementFn(desired)
	if err != nil {
		if repairErr := s.repairTunnel(current); repairErr != nil {
			return false, fmt.Errorf("starting replacement: %v; restoring current tunnel: %w", err, repairErr)
		}
		return false, fmt.Errorf("starting replacement: %w", err)
	}
	if state.OwnerID != s.state.OwnerID || state.GenerationID != desired.GenerationID || state.BuildID != desired.BuildID {
		_ = (realVCSBrokerRuntime{}).ForceStop(s.backend, current.Profile, state)
		if repairErr := s.repairTunnel(current); repairErr != nil {
			return false, fmt.Errorf("replacement published mismatched identity; restoring current tunnel: %w", repairErr)
		}
		return false, fmt.Errorf("replacement published mismatched identity")
	}
	_ = s.server.Close()
	removeVCSBrokerGenerationFiles(current)
	return true, nil
}

func (s *runningVCSBrokerService) applyMapperConfig(desired vcsBrokerServiceConfig, resume bool) error {
	syncBroker, err := newWorkspaceBroker()
	if err != nil {
		return err
	}
	mapper, err := vcsbroker.NewMapper(desired.GuestHome, desired.Specs)
	if err != nil {
		return err
	}
	if err := deployVCSBrokerGuestFn(s.backend, desired.Profile, vcsBrokerGuestPort, s.state.Token, s.state.GenerationID); err != nil {
		return err
	}
	runner, err := newVCSBrokerHostRunnerFn()
	if err != nil {
		return err
	}
	s.server.SetProxy(vcsbroker.NewProxy(syncBroker, mapper, runner))
	s.state.ConfigHash = desired.ConfigHash
	s.state.Phase = ""
	if err := saveRunningVCSBrokerState(desired.StatePath, &s.state); err != nil {
		return err
	}
	if resume {
		s.server.Resume()
	}
	return nil
}

func vcsBrokerLegacyTunnelPath(profile string) (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state", fmt.Sprintf("tunnel-vcs-broker-%s.pid", profile)), nil
}

func (s *runningVCSBrokerService) retireRecreatedLegacyTunnel(cfg vcsBrokerServiceConfig) error {
	path, err := vcsBrokerLegacyTunnelPath(cfg.Profile)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading released-session reverse tunnel record: %w", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	store := vcsbroker.NewStateStoreForPath(cfg.StatePath, vcsBrokerLockWait)
	locked, err := store.Lock(context.Background())
	if err != nil {
		return err
	}
	defer locked.Close()
	if s.legacyRecordNamesOwnedTunnel(pid) {
		fmt.Fprintf(os.Stderr, "VCS broker legacy tunnel record names the service's own tunnel PID %d for profile %q; leaving it unsignaled\n", pid, cfg.Profile)
		return nil
	}
	retired, err := retireLegacyVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", vcsBrokerGuestPort, s.backend.SSHConfig(cfg.Profile))
	if err != nil {
		return fmt.Errorf("migrating legacy VCS broker tunnel: %w", err)
	}
	if retired {
		fmt.Fprintf(os.Stderr, "VCS broker retired released-session reverse tunnel PID %d for profile %q without changing the daemon or token\n", pid, cfg.Profile)
		return nil
	}
	fmt.Fprintf(os.Stderr, "VCS broker removed dead released-session reverse tunnel record PID %d for profile %q\n", pid, cfg.Profile)
	return nil
}

func (s *runningVCSBrokerService) legacyRecordNamesOwnedTunnel(pid int) bool {
	if pid <= 0 || s.tunnel.PID <= 0 || s.tunnel.ProcessIdentity.StartTime == "" {
		return false
	}
	if processidentity.Observe(s.tunnel.PID, s.tunnel.ProcessIdentity).State != processidentity.Ours {
		return false
	}
	if pid == s.tunnel.PID {
		return true
	}
	if s.tunnel.HostPort <= 0 {
		return false
	}
	command, err := vcsBrokerProcessCommandFn(pid)
	if err != nil {
		return false
	}
	hostPort, ok := reverseForwardHostPort(command, vcsBrokerGuestPort)
	return ok && hostPort == s.tunnel.HostPort
}

func (s *runningVCSBrokerService) clearLegacyRecordIfPID(cfg vcsBrokerServiceConfig, pid int) error {
	if pid <= 0 {
		return nil
	}
	path, err := vcsBrokerLegacyTunnelPath(cfg.Profile)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading released-session reverse tunnel record: %w", err)
	}
	recorded, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || recorded != pid {
		return s.retireRecreatedLegacyTunnel(cfg)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing released-session reverse tunnel record: %w", err)
	}
	return nil
}

func reverseForwardHostPort(command string, guestPort int) (int, bool) {
	fields := strings.Fields(command)
	prefix := fmt.Sprintf("%d:127.0.0.1:", guestPort)
	for i, field := range fields {
		if field == "-R" && i+1 < len(fields) && strings.HasPrefix(fields[i+1], prefix) {
			hostPort, err := strconv.Atoi(strings.TrimPrefix(fields[i+1], prefix))
			return hostPort, err == nil && hostPort > 0 && hostPort <= 65535
		}
	}
	return 0, false
}

func (s *runningVCSBrokerService) repairTunnel(cfg vcsBrokerServiceConfig) error {
	ownedPID := s.tunnel.PID
	if err := s.retireRecreatedLegacyTunnel(cfg); err != nil {
		return err
	}
	stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", s.tunnel)
	if err := s.clearLegacyRecordIfPID(cfg, ownedPID); err != nil {
		return err
	}
	claim, err := startVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", s.state.GenerationID, s.state.HostPort, vcsBrokerGuestPort, s.backend.SSHConfig(cfg.Profile))
	if err != nil {
		return err
	}
	s.tunnel = claim
	s.state.TunnelPID = claim.PID
	s.state.TunnelIdentity = claim.ProcessIdentity
	s.state.TunnelTarget = claim.Target
	if err := deployVCSBrokerGuestFn(s.backend, cfg.Profile, vcsBrokerGuestPort, s.state.Token, s.state.GenerationID); err != nil {
		stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", claim)
		return err
	}
	if !probeVCSBrokerGuestWithRetryFn(s.backend, cfg.Profile, vcsBrokerGuestPort, s.state.Token, s.state.GenerationID) {
		vcsbroker.RemoveGuestConfig(s.backend, cfg.Profile, s.state.GenerationID)
		stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", claim)
		return fmt.Errorf("repaired VCS broker tunnel failed its authenticated guest health check")
	}
	return saveRunningVCSBrokerState(cfg.StatePath, &s.state)
}

func saveRunningVCSBrokerState(statePath string, state *vcsbroker.ServiceState) error {
	store := vcsbroker.NewStateStoreForPath(statePath, vcsBrokerLockWait)
	locked, err := store.Lock(context.Background())
	if err != nil {
		return err
	}
	defer locked.Close()
	current, err := locked.Load()
	if err != nil {
		return err
	}
	if current.OwnerID != state.OwnerID || current.GenerationID != state.GenerationID {
		return fmt.Errorf("refusing to publish VCS broker state for a non-current generation")
	}
	state.EnsureHelperPID = current.EnsureHelperPID
	state.EnsureHelperIdentity = current.EnsureHelperIdentity
	state.EnsureError = current.EnsureError
	state.EnsureCompletedAt = current.EnsureCompletedAt
	return locked.Save(*state)
}

func (s *runningVCSBrokerService) shutdown(cfg vcsBrokerServiceConfig) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.DrainWait)
	commands, err := s.server.Drain(ctx)
	cancel()
	if err != nil {
		_ = writePrivateJSON(cfg.DrainPath, vcsBrokerDrainReport{OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, Commands: commands})
		_ = s.server.Close()
	}
	vcsbroker.RemoveGuestConfig(s.backend, cfg.Profile, s.state.GenerationID)
	stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", s.tunnel)
	_ = s.server.Close()
	if err != nil {
		// All host commands are descendants of this standalone process group.
		// Forced drain expiry kills the group after publishing the exact active
		// commands and cleaning the guest/tunnel ownership.
		_ = processidentity.SignalProcessGroup(os.Getpid(), os.Getpid(), s.state.BrokerIdentity, syscall.SIGKILL)
	}
}

func removeVCSBrokerStateIfOwner(path, profile, ownerID, generationID string) {
	store := vcsbroker.NewStateStore(filepath.Dir(path), profile, vcsBrokerLockWait)
	if store.StatePath != path {
		return
	}
	locked, err := store.Lock(context.Background())
	if err != nil {
		return
	}
	defer locked.Close()
	state, err := locked.Load()
	if err == nil && state.OwnerID == ownerID && state.GenerationID == generationID {
		_ = locked.Remove()
	}
}
