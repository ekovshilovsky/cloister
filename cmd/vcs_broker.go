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
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/tunnel"
	"cloister.io/internal/vcsbroker"
	"cloister.io/internal/vm"
	"github.com/spf13/cobra"
)

const (
	vcsBrokerGuestPort     = 49231
	vcsBrokerLockWait      = 12 * time.Second
	vcsBrokerStartupWait   = 10 * time.Second
	vcsBrokerDrainWait     = 10 * time.Minute
	vcsBrokerShutdownWait  = vcsBrokerDrainWait + 5*time.Second
	vcsBrokerProgressEvery = 5 * time.Second
	vcsBrokerVMCheckEvery  = 15 * time.Second
	vcsBrokerVMMisses      = 3
)

type vcsBrokerServiceConfig struct {
	OwnerID    string                 `json:"owner_id"`
	Profile    string                 `json:"profile"`
	Backend    string                 `json:"backend"`
	GuestHome  string                 `json:"guest_home"`
	Specs      []broker.SessionSpec   `json:"specs"`
	Workspace  config.WorkspaceConfig `json:"workspace"`
	ConfigHash string                 `json:"config_hash"`
	BuildID    string                 `json:"build_id"`
	StatePath  string                 `json:"state_path"`
	ReadyPath  string                 `json:"ready_path"`
	RepairPath string                 `json:"repair_path"`
	DrainPath  string                 `json:"drain_path"`
	ConfigPath string                 `json:"config_path"`
	LogPath    string                 `json:"log_path"`
	DrainWait  time.Duration          `json:"drain_wait"`
}

type vcsBrokerReady struct {
	OwnerID   string `json:"owner_id,omitempty"`
	BrokerPID int    `json:"broker_pid,omitempty"`
	Error     string `json:"error,omitempty"`
}

type vcsBrokerRepairReady struct {
	OwnerID   string `json:"owner_id,omitempty"`
	TunnelPID int    `json:"tunnel_pid,omitempty"`
	Error     string `json:"error,omitempty"`
}

type vcsBrokerDrainReport struct {
	OwnerID  string                    `json:"owner_id"`
	Commands []vcsbroker.ActiveCommand `json:"commands"`
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
	Host   vcsbroker.HostProbeStatus
	Tunnel bool
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
var deployVCSBrokerGuestFn = vcsbroker.DeployGuest
var probeVCSBrokerGuestFn = vcsbroker.ProbeGuest
var newVCSBrokerHostRunnerFn = newVCSBrokerHostRunner
var startVCSBrokerReplacementFn = (realVCSBrokerRuntime{}).Start

func ensureVCSBrokerWithWarning(backend vm.Backend, profile string, p *config.Profile) {
	if err := ensureVCSBrokerFn(backend, profile, p); err != nil {
		fmt.Fprintf(os.Stderr, "warning: VCS broker unavailable for profile %q: %v; VM access will continue; retry with 'cloister repair %s'\n", profile, err, profile)
	}
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
	state, err := locked.Load()
	if err != nil {
		return err
	}
	if state.OwnerID == "" {
		return locked.Remove()
	}
	state.Phase = "retiring"
	if err := locked.Save(state); err != nil {
		return err
	}
	if err := m.runtime.RequestShutdown(state); err == nil {
		return nil
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

	state, err := locked.Load()
	if err != nil {
		return err
	}
	if validVCSBrokerState(state) {
		health := m.runtime.Inspect(backend, profile, state)
		if health.Host == vcsbroker.HostProbeDraining {
			return fmt.Errorf("VCS broker is stopping or restarting; retry with 'cloister repair %s'", profile)
		}
		if health.Host == vcsbroker.HostProbeHealthy {
			if state.ConfigHash != configHash || state.BuildID != m.buildID {
				desired := serviceConfigForExisting(state, profile, backendName, guestHome, p.Workspace, specs, configHash, m.buildID)
				if err := m.runtime.RequestRestart(desired, state); err != nil {
					return fmt.Errorf("requesting idle VCS broker restart: %w", err)
				}
				return nil
			}
			if health.Tunnel {
				return nil
			}
			if err := m.runtime.RequestTunnelRepair(state); err != nil {
				return fmt.Errorf("requesting VCS broker tunnel repair: %w", err)
			}
			return fmt.Errorf("VCS broker tunnel repair requested; retry with 'cloister repair %s'", profile)
		}
	}
	if state.OwnerID != "" {
		stopErr := m.runtime.ForceStop(backend, profile, state)
		if err := locked.Remove(); err != nil {
			return err
		}
		if stopErr != nil {
			return fmt.Errorf("stopping stale VCS broker: %w", stopErr)
		}
	}

	ownerID, err := m.newID()
	if err != nil {
		return fmt.Errorf("creating VCS broker owner identity: %w", err)
	}
	serviceConfig := newVCSBrokerServiceConfig(m.stateDir, store.StatePath, ownerID, profile, backendName, guestHome, p.Workspace, specs, configHash, m.buildID)
	state, err = m.runtime.Start(serviceConfig)
	if err != nil {
		return err
	}
	recorded, err := locked.Load()
	if err != nil {
		_ = m.runtime.Stop(backend, profile, state)
		return err
	}
	if recorded != state || state.OwnerID != ownerID || state.ConfigHash != configHash || state.BuildID != m.buildID {
		_ = m.runtime.Stop(backend, profile, state)
		return fmt.Errorf("VCS broker service published mismatched ownership state")
	}
	return nil
}

func (m *vcsBrokerManager) stop(backend vm.Backend, profile string) error {
	store := vcsbroker.NewStateStore(m.stateDir, profile, m.lockWait)
	locked, err := store.Lock(context.Background())
	if err != nil {
		return err
	}
	state, err := locked.Load()
	if err != nil {
		_ = locked.Close()
		return err
	}
	if state.OwnerID == "" {
		err := locked.Remove()
		_ = locked.Close()
		return err
	}
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
	if current.OwnerID == state.OwnerID {
		if err := locked.Remove(); err != nil {
			return err
		}
	}
	return stopErr
}

func newVCSBrokerServiceConfig(stateDir, statePath, ownerID, profile, backendName, guestHome string, workspace config.WorkspaceConfig, specs []broker.SessionSpec, configHash, buildID string) vcsBrokerServiceConfig {
	base := filepath.Join(stateDir, "vcs-broker-service-"+ownerID)
	return vcsBrokerServiceConfig{
		OwnerID: ownerID, Profile: profile, Backend: backendName,
		GuestHome: guestHome, Specs: specs, Workspace: workspace, ConfigHash: configHash, BuildID: buildID,
		StatePath: statePath, ReadyPath: base + ".ready.json", RepairPath: base + ".repair.json",
		DrainPath: base + ".drain.json", ConfigPath: base + ".json", LogPath: base + ".log", DrainWait: vcsBrokerDrainWait,
	}
}

func serviceConfigForExisting(state vcsbroker.ServiceState, profile, backendName, guestHome string, workspace config.WorkspaceConfig, specs []broker.SessionSpec, configHash, buildID string) vcsBrokerServiceConfig {
	return vcsBrokerServiceConfig{
		OwnerID: state.OwnerID, Profile: profile, Backend: backendName,
		GuestHome: guestHome, Specs: specs, Workspace: workspace, ConfigHash: configHash, BuildID: buildID,
		StatePath: state.StatePath, ReadyPath: state.ReadyPath, RepairPath: state.RepairPath,
		DrainPath: state.DrainPath, ConfigPath: state.ConfigPath, LogPath: state.LogPath, DrainWait: vcsBrokerDrainWait,
	}
}

func validVCSBrokerState(state vcsbroker.ServiceState) bool {
	return state.OwnerID != "" && state.BrokerPID > 0 && state.TunnelPID > 0 &&
		state.HostPort > 0 && state.GuestPort == vcsBrokerGuestPort && state.Token != "" &&
		state.ConfigHash != "" && state.BuildID != "" && state.TunnelTarget != "" && state.StatePath != "" && state.ConfigPath != "" &&
		state.ReadyPath != "" && state.RepairPath != "" && state.DrainPath != "" && state.LogPath != ""
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
	if !vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID) {
		return vcsBrokerHealth{}
	}
	hostStatus := vcsbroker.ProbeHost(state.HostPort, state.Token)
	if hostStatus != vcsbroker.HostProbeHealthy {
		return vcsBrokerHealth{Host: hostStatus}
	}
	claim := tunnel.ReverseForwardOwner{
		OwnerID: state.OwnerID, PID: state.TunnelPID, HostPort: state.HostPort,
		GuestPort: state.GuestPort, Target: state.TunnelTarget,
	}
	return vcsBrokerHealth{
		Host: vcsbroker.HostProbeHealthy,
		Tunnel: tunnel.OwnedReverseForwardHealthy(profile, "vcs-broker", claim) &&
			probeVCSBrokerGuestFn(backend, profile, state.GuestPort, state.Token, state.OwnerID),
	}
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
	command := exec.Command(executable, "vcs-broker", "serve", cfg.Profile, "--config", cfg.ConfigPath, "--owner", cfg.OwnerID)
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
	_ = command.Process.Release()
	_ = logFile.Close()
	failed := true
	defer func() {
		if failed {
			cleanupFailedVCSBrokerStart(cfg, pid)
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
			state, err := vcsbroker.ReadServiceState(cfg.StatePath)
			if err != nil {
				return vcsbroker.ServiceState{}, err
			}
			if ready.OwnerID != cfg.OwnerID || ready.BrokerPID != pid || state.OwnerID != cfg.OwnerID || state.BrokerPID != pid {
				return vcsbroker.ServiceState{}, fmt.Errorf("VCS broker readiness did not match the launched owner")
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
	_ = stopVCSBrokerProcess(pid, cfg.OwnerID)
	return vcsbroker.ServiceState{}, fmt.Errorf("timed out after %s starting VCS broker service", vcsBrokerStartupWait)
}

func cleanupFailedVCSBrokerStart(cfg vcsBrokerServiceConfig, pid int) {
	if vcsBrokerProcessMatches(pid, cfg.OwnerID) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if state, err := vcsbroker.ReadServiceState(cfg.StatePath); err == nil && state.OwnerID == cfg.OwnerID && state.BrokerPID == pid {
		if backend, resolveErr := resolveBackend(cfg.Backend); resolveErr == nil {
			vcsbroker.RemoveGuestConfig(backend, cfg.Profile, cfg.OwnerID)
		}
		claim := tunnel.ReverseForwardOwner{
			OwnerID: state.OwnerID, PID: state.TunnelPID, HostPort: state.HostPort,
			GuestPort: state.GuestPort, Target: state.TunnelTarget,
		}
		tunnel.StopOwnedReverseForward(cfg.Profile, "vcs-broker", claim)
		_ = os.Remove(cfg.StatePath)
	}
	for _, path := range []string{cfg.ConfigPath, cfg.ReadyPath, cfg.RepairPath, cfg.DrainPath, cfg.LogPath} {
		_ = os.Remove(path)
	}
}

func (realVCSBrokerRuntime) RequestTunnelRepair(state vcsbroker.ServiceState) error {
	if !vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID) {
		return fmt.Errorf("VCS broker daemon is not running")
	}
	_ = os.Remove(state.RepairPath)
	if err := syscall.Kill(state.BrokerPID, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("requesting tunnel repair: %w", err)
	}
	return nil
}

func (realVCSBrokerRuntime) RequestRestart(cfg vcsBrokerServiceConfig, state vcsbroker.ServiceState) error {
	if !vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID) {
		return fmt.Errorf("VCS broker daemon is not running")
	}
	if err := writePrivateJSON(cfg.ConfigPath, cfg); err != nil {
		return fmt.Errorf("writing deferred VCS broker configuration: %w", err)
	}
	if err := syscall.Kill(state.BrokerPID, syscall.SIGUSR2); err != nil {
		return fmt.Errorf("requesting deferred VCS broker restart: %w", err)
	}
	return nil
}

func (realVCSBrokerRuntime) RequestShutdown(state vcsbroker.ServiceState) error {
	if !vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID) {
		return fmt.Errorf("VCS broker daemon is not running")
	}
	if err := syscall.Kill(state.BrokerPID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("requesting VCS broker shutdown: %w", err)
	}
	return nil
}

func (realVCSBrokerRuntime) Stop(backend vm.Backend, profile string, state vcsbroker.ServiceState) error {
	_ = os.Remove(state.DrainPath)
	pid := state.BrokerPID
	if !vcsBrokerProcessMatches(pid, state.OwnerID) {
		pid = findVCSBrokerOwnerPID(state.OwnerID)
	}
	if pid > 0 && vcsBrokerProcessMatches(pid, state.OwnerID) {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("signaling VCS broker service: %w", err)
		}
		deadline := time.Now().Add(vcsBrokerShutdownWait)
		startedWaiting := time.Now()
		nextProgress := startedWaiting.Add(vcsBrokerProgressEvery)
		for vcsBrokerProcessAlive(pid) && time.Now().Before(deadline) {
			if !time.Now().Before(nextProgress) {
				if status, statusErr := vcsbroker.ReadHostStatus(state.HostPort, state.Token); statusErr == nil && len(status.Commands) > 0 {
					writeVCSBrokerStopProgress(os.Stderr, time.Since(startedWaiting), status.Commands)
				}
				nextProgress = time.Now().Add(vcsBrokerProgressEvery)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if vcsBrokerProcessAlive(pid) {
			if err := stopVCSBrokerProcess(pid, state.OwnerID); err != nil {
				return err
			}
		}
	}
	vcsbroker.RemoveGuestConfig(backend, profile, state.OwnerID)
	claim := tunnel.ReverseForwardOwner{
		OwnerID: state.OwnerID, PID: state.TunnelPID, HostPort: state.HostPort,
		GuestPort: state.GuestPort, Target: state.TunnelTarget,
	}
	tunnel.StopOwnedReverseForward(profile, "vcs-broker", claim)
	var drainErr error
	if data, err := os.ReadFile(state.DrainPath); err == nil {
		var report vcsBrokerDrainReport
		if json.Unmarshal(data, &report) == nil && report.OwnerID == state.OwnerID && len(report.Commands) > 0 {
			drainErr = formatVCSBrokerDrainError(report.Commands)
		}
	}
	for _, path := range []string{state.ConfigPath, state.ReadyPath, state.RepairPath, state.DrainPath, state.LogPath} {
		_ = os.Remove(path)
	}
	return drainErr
}

func writeVCSBrokerStopProgress(out io.Writer, waited time.Duration, commands []vcsbroker.ActiveCommand) {
	fmt.Fprintf(out, "Waiting %s for VCS broker commands to finish: %s\n",
		waited.Round(time.Second), describeVCSBrokerCommands(commands))
}

func (realVCSBrokerRuntime) ForceStop(backend vm.Backend, profile string, state vcsbroker.ServiceState) error {
	pid := state.BrokerPID
	if !vcsBrokerProcessMatches(pid, state.OwnerID) {
		pid = findVCSBrokerOwnerPID(state.OwnerID)
	}
	if pid > 0 && vcsBrokerProcessMatches(pid, state.OwnerID) {
		if err := stopVCSBrokerProcess(pid, state.OwnerID); err != nil {
			return err
		}
	}
	vcsbroker.RemoveGuestConfig(backend, profile, state.OwnerID)
	claim := tunnel.ReverseForwardOwner{
		OwnerID: state.OwnerID, PID: state.TunnelPID, HostPort: state.HostPort,
		GuestPort: state.GuestPort, Target: state.TunnelTarget,
	}
	tunnel.StopOwnedReverseForward(profile, "vcs-broker", claim)
	for _, path := range []string{state.ConfigPath, state.ReadyPath, state.RepairPath, state.DrainPath, state.LogPath} {
		_ = os.Remove(path)
	}
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

func vcsBrokerProcessMatches(pid int, ownerID string) bool {
	if pid <= 0 || ownerID == "" || !vcsBrokerProcessAlive(pid) {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	return adjacentCommandFields(fields, "vcs-broker", "serve") && commandFlagEquals(fields, "--owner", ownerID)
}

func findVCSBrokerOwnerPID(ownerID string) int {
	if ownerID == "" {
		return 0
	}
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if !adjacentCommandFields(fields, "vcs-broker", "serve") || !commandFlagEquals(fields, "--owner", ownerID) {
			continue
		}
		if len(fields) == 0 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		if pid > 0 {
			return pid
		}
	}
	return 0
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

func stopVCSBrokerProcess(pid int, ownerID string) error {
	if !vcsBrokerProcessMatches(pid, ownerID) {
		return fmt.Errorf("refusing to kill VCS broker PID %d without owner %s", pid, ownerID)
	}
	// The standalone daemon starts a new session and its VCS children inherit
	// that process group. A forced stop targets the group so a child cannot
	// continue mutating a repository after the broker is gone.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
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
		return runVCSBrokerService(args[0], vcsBrokerServeConfig, vcsBrokerServeOwner)
	},
}

var vcsBrokerWatchChildCmd = &cobra.Command{
	Use:   "watch-child <process-group> <child-pid>",
	Short: "Watch one internal broker child process",
	Long: `Internal watchdog started by the VCS broker beside each accepted host
command. If the owning daemon disappears, it kills the daemon's private process
group so the command cannot continue mutating a repository without supervision.`,
	Hidden: true,
	Args:   cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		processGroup, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid VCS broker process group")
		}
		childPID, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid VCS child PID")
		}
		return runVCSBrokerChildWatchdog(processGroup, childPID)
	},
}

func init() {
	vcsBrokerServeCmd.Flags().StringVar(&vcsBrokerServeConfig, "config", "", "internal service configuration")
	vcsBrokerServeCmd.Flags().StringVar(&vcsBrokerServeOwner, "owner", "", "internal owner identity")
	vcsBrokerCmd.AddCommand(vcsBrokerServeCmd, vcsBrokerWatchChildCmd)
	rootCmd.AddCommand(vcsBrokerCmd)
}

func runVCSBrokerChildWatchdog(processGroup, childPID int) error {
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
	// EOF means the broker disappeared without stopping this watcher. Kill the
	// private process group so the host command cannot outlive its authority.
	// SIGSTOP deliberately does not trigger this path: the broker still owns
	// the pipe, and letting an accepted repository operation reach its barrier
	// is safer than asynchronously killing it midway through mutation.
	_ = syscall.Kill(-processGroup, syscall.SIGKILL)
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

func runVCSBrokerService(profile, configPath, ownerID string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg vcsBrokerServiceConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if profile == "" || cfg.Profile != profile || ownerID == "" || cfg.OwnerID != ownerID || cfg.ConfigPath != configPath {
		return fmt.Errorf("VCS broker service identity does not match its configuration")
	}
	service, err := startVCSBrokerService(cfg)
	if err != nil {
		_ = writePrivateJSON(cfg.ReadyPath, vcsBrokerReady{Error: err.Error()})
		return err
	}
	if err := vcsbroker.WriteServiceState(cfg.StatePath, service.state); err != nil {
		service.shutdown(cfg)
		return err
	}
	if err := writePrivateJSON(cfg.ReadyPath, vcsBrokerReady{OwnerID: ownerID, BrokerPID: os.Getpid()}); err != nil {
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
	state   vcsbroker.ServiceState
	backend vm.Backend
	server  *vcsbroker.Server
	tunnel  tunnel.ReverseForwardOwner
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
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(syncBroker, mapper, runner), token)
	if err != nil {
		return nil, err
	}
	claim, err := startVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", cfg.OwnerID, server.Port(), vcsBrokerGuestPort, backend.SSHConfig(cfg.Profile))
	if err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("starting VCS broker tunnel: %w", err)
	}
	state := vcsbroker.ServiceState{
		OwnerID: cfg.OwnerID, BrokerPID: os.Getpid(), TunnelPID: claim.PID,
		HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: token,
		ConfigHash: cfg.ConfigHash, BuildID: cfg.BuildID, TunnelTarget: claim.Target,
		StatePath: cfg.StatePath, ConfigPath: cfg.ConfigPath, ReadyPath: cfg.ReadyPath,
		RepairPath: cfg.RepairPath, DrainPath: cfg.DrainPath, LogPath: cfg.LogPath,
	}
	if err := deployVCSBrokerGuestFn(backend, cfg.Profile, vcsBrokerGuestPort, token, cfg.OwnerID); err != nil {
		stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", claim)
		_ = server.Close()
		return nil, err
	}
	deadline := time.Now().Add(3 * time.Second)
	for !probeVCSBrokerGuestFn(backend, cfg.Profile, vcsBrokerGuestPort, token, cfg.OwnerID) {
		if time.Now().After(deadline) {
			vcsbroker.RemoveGuestConfig(backend, cfg.Profile, cfg.OwnerID)
			stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", claim)
			_ = server.Close()
			return nil, fmt.Errorf("VCS broker tunnel failed its authenticated guest health check")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &runningVCSBrokerService{state: state, backend: backend, server: server, tunnel: claim}, nil
}

type vcsBrokerTransitionResult struct {
	commands []vcsbroker.ActiveCommand
	err      error
}

func runVCSBrokerServiceLoop(service *runningVCSBrokerService, cfg vcsBrokerServiceConfig, signals <-chan os.Signal, ticks, _ <-chan time.Time) error {
	misses := 0
	restartPending := false
	var transition <-chan vcsBrokerTransitionResult
	for {
		select {
		case received := <-signals:
			if received == syscall.SIGUSR1 {
				// The reverse forward is transport, not command execution state.
				// Replacing a broken tunnel must not wait behind host work.
				err := service.repairTunnel(cfg)
				result := vcsBrokerRepairReady{OwnerID: cfg.OwnerID, TunnelPID: service.state.TunnelPID}
				if err != nil {
					result.Error = err.Error()
				}
				_ = writePrivateJSON(cfg.RepairPath, result)
				continue
			}
			if received == syscall.SIGUSR2 {
				restartPending = true
				if transition == nil {
					result := make(chan vcsBrokerTransitionResult, 1)
					transition = result
					go drainVCSBrokerTransition(service.server, cfg.DrainWait, result)
				}
				continue
			}
			service.shutdown(cfg)
			if vcsBrokerStateHasPhase(cfg.StatePath, cfg.OwnerID, "retiring") {
				logVCSBrokerDrainReport(cfg.DrainPath, cfg.OwnerID)
				removeVCSBrokerStateIfOwner(cfg.StatePath, cfg.Profile, cfg.OwnerID)
				removeVCSBrokerAuxiliaryFiles(cfg)
			}
			return nil
		case result := <-transition:
			transition = nil
			if result.err != nil {
				fmt.Fprintf(os.Stderr, "VCS broker deferred restart exceeded %s while draining %s; continuing with the current broker\n", cfg.DrainWait, describeVCSBrokerCommands(result.commands))
				service.server.Resume()
				restartPending = false
				continue
			}
			if !restartPending {
				service.server.Resume()
				continue
			}
			desired, err := readVCSBrokerServiceConfig(cfg.ConfigPath, cfg.OwnerID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "VCS broker deferred restart: %v\n", err)
				service.server.Resume()
				restartPending = false
				continue
			}
			replaced, err := service.applyDesiredConfig(cfg, desired)
			if err != nil {
				fmt.Fprintf(os.Stderr, "VCS broker deferred restart: %v\n", err)
				service.server.Resume()
				restartPending = false
				continue
			}
			if replaced {
				return nil
			}
			cfg = desired
			restartPending = false
		case <-ticks:
			if service.backend.IsRunning(cfg.Profile) {
				misses = 0
				continue
			}
			misses++
			if misses < vcsBrokerVMMisses {
				continue
			}
			service.shutdown(cfg)
			logVCSBrokerDrainReport(cfg.DrainPath, cfg.OwnerID)
			removeVCSBrokerStateIfOwner(cfg.StatePath, cfg.Profile, cfg.OwnerID)
			removeVCSBrokerAuxiliaryFiles(cfg)
			return nil
		}
	}
}

func drainVCSBrokerTransition(server *vcsbroker.Server, wait time.Duration, result chan<- vcsBrokerTransitionResult) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	commands, err := server.Pause(ctx)
	result <- vcsBrokerTransitionResult{commands: commands, err: err}
}

func vcsBrokerStateHasPhase(path, ownerID, phase string) bool {
	state, err := vcsbroker.ReadServiceState(path)
	return err == nil && state.OwnerID == ownerID && state.Phase == phase
}

func logVCSBrokerDrainReport(path, ownerID string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var report vcsBrokerDrainReport
	if json.Unmarshal(data, &report) == nil && report.OwnerID == ownerID && len(report.Commands) > 0 {
		fmt.Fprintf(os.Stderr, "%v\n", formatVCSBrokerDrainError(report.Commands))
	}
}

func removeVCSBrokerAuxiliaryFiles(cfg vcsBrokerServiceConfig) {
	for _, path := range []string{cfg.ConfigPath, cfg.ReadyPath, cfg.RepairPath, cfg.DrainPath, cfg.LogPath} {
		_ = os.Remove(path)
	}
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
	if cfg.OwnerID != ownerID || cfg.ConfigPath != path || cfg.DrainWait <= 0 {
		return vcsBrokerServiceConfig{}, fmt.Errorf("deferred service identity mismatch")
	}
	return cfg, nil
}

func (s *runningVCSBrokerService) applyDesiredConfig(current, desired vcsBrokerServiceConfig) (bool, error) {
	hash, err := vcsBrokerConfigHash(desired.GuestHome, desired.Workspace, desired.Specs)
	if err != nil || hash != desired.ConfigHash {
		return false, fmt.Errorf("deferred service configuration hash mismatch")
	}
	if desired.BuildID == s.state.BuildID {
		syncBroker, err := newWorkspaceBroker()
		if err != nil {
			return false, err
		}
		mapper, err := vcsbroker.NewMapper(desired.GuestHome, desired.Specs)
		if err != nil {
			return false, err
		}
		if err := deployVCSBrokerGuestFn(s.backend, desired.Profile, vcsBrokerGuestPort, s.state.Token, desired.OwnerID); err != nil {
			return false, err
		}
		runner, err := newVCSBrokerHostRunnerFn()
		if err != nil {
			return false, err
		}
		s.server.SetProxy(vcsbroker.NewProxy(syncBroker, mapper, runner))
		s.state.ConfigHash = desired.ConfigHash
		s.state.Phase = ""
		if err := vcsbroker.WriteServiceState(desired.StatePath, s.state); err != nil {
			return false, err
		}
		s.server.Resume()
		return false, nil
	}

	store := vcsbroker.NewStateStore(filepath.Dir(current.StatePath), current.Profile, vcsBrokerLockWait)
	if store.StatePath != current.StatePath {
		return false, fmt.Errorf("replacement service state path mismatch")
	}
	locked, err := store.Lock(context.Background())
	if err != nil {
		return false, fmt.Errorf("locking idle VCS broker replacement: %w", err)
	}
	defer locked.Close()
	recorded, err := locked.Load()
	if err != nil || recorded.OwnerID != s.state.OwnerID || recorded.BrokerPID != s.state.BrokerPID {
		return false, fmt.Errorf("VCS broker ownership changed before idle replacement")
	}
	stopVCSBrokerTunnelFn(current.Profile, "vcs-broker", s.tunnel)
	state, err := startVCSBrokerReplacementFn(desired)
	if err != nil {
		_ = writePrivateJSON(current.ConfigPath, current)
		if repairErr := s.repairTunnel(current); repairErr != nil {
			return false, fmt.Errorf("starting replacement: %v; restoring current tunnel: %w", err, repairErr)
		}
		return false, fmt.Errorf("starting replacement: %w", err)
	}
	if state.OwnerID != s.state.OwnerID || state.BuildID != desired.BuildID {
		_ = (realVCSBrokerRuntime{}).ForceStop(s.backend, current.Profile, state)
		_ = writePrivateJSON(current.ConfigPath, current)
		if repairErr := s.repairTunnel(current); repairErr != nil {
			return false, fmt.Errorf("replacement published mismatched identity; restoring current tunnel: %w", repairErr)
		}
		return false, fmt.Errorf("replacement published mismatched identity")
	}
	_ = s.server.Close()
	return true, nil
}

func (s *runningVCSBrokerService) repairTunnel(cfg vcsBrokerServiceConfig) error {
	stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", s.tunnel)
	claim, err := startVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", cfg.OwnerID, s.state.HostPort, vcsBrokerGuestPort, s.backend.SSHConfig(cfg.Profile))
	if err != nil {
		return err
	}
	s.tunnel = claim
	s.state.TunnelPID = claim.PID
	s.state.TunnelTarget = claim.Target
	if err := deployVCSBrokerGuestFn(s.backend, cfg.Profile, vcsBrokerGuestPort, s.state.Token, cfg.OwnerID); err != nil {
		stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", claim)
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for !probeVCSBrokerGuestFn(s.backend, cfg.Profile, vcsBrokerGuestPort, s.state.Token, cfg.OwnerID) {
		if time.Now().After(deadline) {
			vcsbroker.RemoveGuestConfig(s.backend, cfg.Profile, cfg.OwnerID)
			stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", claim)
			return fmt.Errorf("repaired VCS broker tunnel failed its authenticated guest health check")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return vcsbroker.WriteServiceState(cfg.StatePath, s.state)
}

func (s *runningVCSBrokerService) shutdown(cfg vcsBrokerServiceConfig) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.DrainWait)
	commands, err := s.server.Drain(ctx)
	cancel()
	if err != nil {
		_ = writePrivateJSON(cfg.DrainPath, vcsBrokerDrainReport{OwnerID: cfg.OwnerID, Commands: commands})
		_ = s.server.Close()
	}
	vcsbroker.RemoveGuestConfig(s.backend, cfg.Profile, s.state.OwnerID)
	stopVCSBrokerTunnelFn(cfg.Profile, "vcs-broker", s.tunnel)
	_ = s.server.Close()
	if err != nil {
		// All host commands are descendants of this standalone process group.
		// Forced drain expiry kills the group after publishing the exact active
		// commands and cleaning the guest/tunnel ownership.
		_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
	}
}

func removeVCSBrokerStateIfOwner(path, profile, ownerID string) {
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
	if err == nil && state.OwnerID == ownerID {
		_ = locked.Remove()
	}
}
