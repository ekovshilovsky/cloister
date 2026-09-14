package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	vcsBrokerGuestPort    = 49231
	vcsBrokerLockWait     = 12 * time.Second
	vcsBrokerStartupWait  = 10 * time.Second
	vcsBrokerShutdownWait = 3 * time.Second
)

type vcsBrokerServiceConfig struct {
	OwnerID    string                 `json:"owner_id"`
	Profile    string                 `json:"profile"`
	Backend    string                 `json:"backend"`
	GuestHome  string                 `json:"guest_home"`
	Specs      []broker.SessionSpec   `json:"specs"`
	Workspace  config.WorkspaceConfig `json:"workspace"`
	ConfigHash string                 `json:"config_hash"`
	StatePath  string                 `json:"state_path"`
	ReadyPath  string                 `json:"ready_path"`
	ConfigPath string                 `json:"config_path"`
	LogPath    string                 `json:"log_path"`
}

type vcsBrokerReady struct {
	OwnerID   string `json:"owner_id,omitempty"`
	BrokerPID int    `json:"broker_pid,omitempty"`
	Error     string `json:"error,omitempty"`
}

type vcsBrokerRuntime interface {
	Healthy(vm.Backend, string, vcsbroker.ServiceState) bool
	Start(vcsBrokerServiceConfig) (vcsbroker.ServiceState, error)
	Stop(vm.Backend, string, vcsbroker.ServiceState) error
}

type vcsBrokerManager struct {
	stateDir string
	lockWait time.Duration
	runtime  vcsBrokerRuntime
	newID    func() (string, error)
}

// ensureVCSBrokerFn is the command-level seam for tests whose concern is not
// the standalone service composition.
var ensureVCSBrokerFn = ensureVCSBroker
var stopVCSBrokerFn = stopVCSBroker

func ensureVCSBroker(backend vm.Backend, profile string, p *config.Profile) error {
	manager, err := newVCSBrokerManager()
	if err != nil {
		return err
	}
	if !workspaceProvider(p).IsBroker() {
		return manager.stop(backend, profile)
	}
	backendName, err := vm.ResolveBackendName(p.Backend)
	if err != nil {
		return err
	}
	return manager.ensure(backend, profile, backendName, p)
}

func stopVCSBroker(backend vm.Backend, profile string) error {
	manager, err := newVCSBrokerManager()
	if err != nil {
		return err
	}
	return manager.stop(backend, profile)
}

func newVCSBrokerManager() (*vcsBrokerManager, error) {
	configDir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	return &vcsBrokerManager{
		stateDir: filepath.Join(configDir, "state"),
		lockWait: vcsBrokerLockWait,
		runtime:  realVCSBrokerRuntime{},
		newID:    newVCSBrokerID,
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
	if validVCSBrokerState(state) && state.ConfigHash == configHash && m.runtime.Healthy(backend, profile, state) {
		return nil
	}
	if state.OwnerID != "" {
		if err := m.runtime.Stop(backend, profile, state); err != nil {
			return fmt.Errorf("stopping stale VCS broker: %w", err)
		}
		if err := locked.Remove(); err != nil {
			return err
		}
	}

	ownerID, err := m.newID()
	if err != nil {
		return fmt.Errorf("creating VCS broker owner identity: %w", err)
	}
	base := filepath.Join(m.stateDir, "vcs-broker-service-"+ownerID)
	serviceConfig := vcsBrokerServiceConfig{
		OwnerID: ownerID, Profile: profile, Backend: backendName,
		GuestHome: guestHome, Specs: specs, Workspace: p.Workspace, ConfigHash: configHash,
		StatePath: store.StatePath, ReadyPath: base + ".ready.json",
		ConfigPath: base + ".json", LogPath: base + ".log",
	}
	state, err = m.runtime.Start(serviceConfig)
	if err != nil {
		return err
	}
	recorded, err := locked.Load()
	if err != nil {
		_ = m.runtime.Stop(backend, profile, state)
		return err
	}
	if recorded != state || state.OwnerID != ownerID || state.ConfigHash != configHash {
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
	defer locked.Close()
	state, err := locked.Load()
	if err != nil {
		return err
	}
	if state.OwnerID == "" {
		return locked.Remove()
	}
	if err := m.runtime.Stop(backend, profile, state); err != nil {
		return err
	}
	return locked.Remove()
}

func validVCSBrokerState(state vcsbroker.ServiceState) bool {
	return state.OwnerID != "" && state.BrokerPID > 0 && state.TunnelPID > 0 &&
		state.HostPort > 0 && state.GuestPort == vcsBrokerGuestPort && state.Token != "" &&
		state.ConfigHash != "" && state.TunnelTarget != "" && state.ConfigPath != "" &&
		state.ReadyPath != "" && state.LogPath != ""
}

func resolveVCSBrokerGuestHome(backend vm.Backend, profile string) (string, error) {
	out, err := backend.SSHCapture(profile, `printf '__CLH[%s]CLH__' "$HOME"`)
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

type realVCSBrokerRuntime struct{}

func (realVCSBrokerRuntime) Healthy(backend vm.Backend, profile string, state vcsbroker.ServiceState) bool {
	claim := tunnel.ReverseForwardOwner{
		OwnerID: state.OwnerID, PID: state.TunnelPID, HostPort: state.HostPort,
		GuestPort: state.GuestPort, Target: state.TunnelTarget,
	}
	return vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID) &&
		tunnel.OwnedReverseForwardHealthy(profile, "vcs-broker", claim) &&
		vcsbroker.ProbeGuest(backend, profile, state.GuestPort, state.Token, state.OwnerID)
}

func (realVCSBrokerRuntime) Start(cfg vcsBrokerServiceConfig) (vcsbroker.ServiceState, error) {
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
	if state, err := vcsbroker.ReadServiceState(cfg.StatePath); err == nil && state.OwnerID == cfg.OwnerID {
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
	for _, path := range []string{cfg.ConfigPath, cfg.ReadyPath, cfg.LogPath} {
		_ = os.Remove(path)
	}
}

func (realVCSBrokerRuntime) Stop(backend vm.Backend, profile string, state vcsbroker.ServiceState) error {
	pid := state.BrokerPID
	if !vcsBrokerProcessMatches(pid, state.OwnerID) {
		pid = findVCSBrokerOwnerPID(state.OwnerID)
	}
	if pid > 0 && vcsBrokerProcessMatches(pid, state.OwnerID) {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("signaling VCS broker service: %w", err)
		}
		deadline := time.Now().Add(vcsBrokerShutdownWait)
		for vcsBrokerProcessAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
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
	for _, path := range []string{state.ConfigPath, state.ReadyPath, state.LogPath} {
		_ = os.Remove(path)
	}
	return nil
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
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
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

var vcsBrokerCmd = &cobra.Command{Use: "vcs-broker", Hidden: true}

var vcsBrokerServeCmd = &cobra.Command{
	Use:    "serve <profile>",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runVCSBrokerService(args[0], vcsBrokerServeConfig, vcsBrokerServeOwner)
	},
}

func init() {
	vcsBrokerServeCmd.Flags().StringVar(&vcsBrokerServeConfig, "config", "", "internal service configuration")
	vcsBrokerServeCmd.Flags().StringVar(&vcsBrokerServeOwner, "owner", "", "internal owner identity")
	vcsBrokerCmd.AddCommand(vcsBrokerServeCmd)
	rootCmd.AddCommand(vcsBrokerCmd)
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
		service.close(cfg.Profile)
		return err
	}
	if err := writePrivateJSON(cfg.ReadyPath, vcsBrokerReady{OwnerID: ownerID, BrokerPID: os.Getpid()}); err != nil {
		service.close(cfg.Profile)
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	<-signals
	service.close(cfg.Profile)
	return nil
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
	syncBroker, err := newWorkspaceBroker()
	if err != nil {
		return nil, err
	}
	mapper, err := vcsbroker.NewMapper(cfg.GuestHome, cfg.Specs)
	if err != nil {
		return nil, err
	}
	token, err := newVCSBrokerID()
	if err != nil {
		return nil, err
	}
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(syncBroker, mapper, nil), token)
	if err != nil {
		return nil, err
	}
	claim, err := tunnel.StartOwnedReverseForward(cfg.Profile, "vcs-broker", cfg.OwnerID, server.Port(), vcsBrokerGuestPort, backend.SSHConfig(cfg.Profile))
	if err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("starting VCS broker tunnel: %w", err)
	}
	state := vcsbroker.ServiceState{
		OwnerID: cfg.OwnerID, BrokerPID: os.Getpid(), TunnelPID: claim.PID,
		HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: token,
		ConfigHash: cfg.ConfigHash, TunnelTarget: claim.Target,
		ConfigPath: cfg.ConfigPath, ReadyPath: cfg.ReadyPath, LogPath: cfg.LogPath,
	}
	if err := vcsbroker.DeployGuest(backend, cfg.Profile, vcsBrokerGuestPort, token, cfg.OwnerID); err != nil {
		tunnel.StopOwnedReverseForward(cfg.Profile, "vcs-broker", claim)
		_ = server.Close()
		return nil, err
	}
	deadline := time.Now().Add(3 * time.Second)
	for !vcsbroker.ProbeGuest(backend, cfg.Profile, vcsBrokerGuestPort, token, cfg.OwnerID) {
		if time.Now().After(deadline) {
			vcsbroker.RemoveGuestConfig(backend, cfg.Profile, cfg.OwnerID)
			tunnel.StopOwnedReverseForward(cfg.Profile, "vcs-broker", claim)
			_ = server.Close()
			return nil, fmt.Errorf("VCS broker tunnel failed its authenticated guest health check")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &runningVCSBrokerService{state: state, backend: backend, server: server, tunnel: claim}, nil
}

func (s *runningVCSBrokerService) close(profile string) {
	if s == nil {
		return
	}
	vcsbroker.RemoveGuestConfig(s.backend, profile, s.state.OwnerID)
	tunnel.StopOwnedReverseForward(profile, "vcs-broker", s.tunnel)
	_ = s.server.Close()
}
