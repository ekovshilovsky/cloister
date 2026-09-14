package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/tunnel"
	"cloister.io/internal/vcsbroker"
	"cloister.io/internal/vm"
)

type fakePersistentVCSRuntime struct {
	mu              sync.Mutex
	starts          int
	stops           []vcsbroker.ServiceState
	repairs         int
	restarts        int
	shutdowns       int
	brokerAlive     bool
	hostHealthy     bool
	tunnelAlive     bool
	endpointHealthy bool
}

type runningSequenceBackend struct {
	vm.MockBackend
	mu     sync.Mutex
	values []bool
}

type blockingCommandRunner struct {
	started chan struct{}
	release chan struct{}
}

type subprocessDrainBarrier struct {
	mu      sync.Mutex
	flushes int
	post    string
	release string
	refresh string
}

type subprocessSpoolRunner struct {
	ready string
}

func (r subprocessSpoolRunner) Run(ctx context.Context, _ string, _ []string, _ string, _ []string, output io.Writer) (int, error) {
	_, _ = io.WriteString(output, "secret output marker\n")
	if err := os.WriteFile(r.ready, []byte("spooled\n"), 0o600); err != nil {
		return 125, err
	}
	<-ctx.Done()
	return 125, ctx.Err()
}

func (b *subprocessDrainBarrier) Create(context.Context, broker.SessionSpec) error { return nil }
func (b *subprocessDrainBarrier) Pause(context.Context, broker.SessionSpec) error  { return nil }
func (b *subprocessDrainBarrier) Resume(context.Context, broker.SessionSpec) error { return nil }
func (b *subprocessDrainBarrier) Terminate(context.Context, broker.SessionSpec) error {
	return nil
}
func (b *subprocessDrainBarrier) Flush(ctx context.Context, _ broker.SessionSpec) error {
	b.mu.Lock()
	b.flushes++
	flush := b.flushes
	b.mu.Unlock()
	if flush != 2 {
		return nil
	}
	if err := os.WriteFile(b.post, []byte("post\n"), 0o600); err != nil {
		return err
	}
	for {
		if _, err := os.Stat(b.release); err == nil {
			return os.WriteFile(b.refresh, []byte("refreshed\n"), 0o600)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (b *subprocessDrainBarrier) Status(_ context.Context, spec broker.SessionSpec) (broker.Status, error) {
	return broker.Status{State: broker.StateActive, HostRoot: spec.HostRoot, GuestRoot: spec.GuestRoot}, nil
}

func (r *blockingCommandRunner) Run(ctx context.Context, _ string, _ []string, _ string, _ []string, output io.Writer) (int, error) {
	close(r.started)
	select {
	case <-r.release:
		_, _ = io.WriteString(output, "completed\n")
		return 0, nil
	case <-ctx.Done():
		return 125, ctx.Err()
	}
}

func (b *runningSequenceBackend) IsRunning(string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.values) == 0 {
		return false
	}
	value := b.values[0]
	b.values = b.values[1:]
	return value
}

type mismatchedPublishRuntime struct {
	*fakePersistentVCSRuntime
}

type blockingStopRuntime struct {
	*fakePersistentVCSRuntime
	started chan struct{}
	release chan struct{}
}

type drainTimeoutRuntime struct {
	*fakePersistentVCSRuntime
}

type detachedProbeRuntime struct {
	mu        sync.Mutex
	starts    int
	stops     int
	processes []*exec.Cmd
}

type unhealthyDetachedRuntime struct {
	pid       int
	forceStop atomic.Bool
}

func (r *unhealthyDetachedRuntime) Inspect(_ vm.Backend, _ string, state vcsbroker.ServiceState) vcsBrokerHealth {
	return vcsBrokerHealth{Host: vcsbroker.ProbeHost(state.HostPort, state.Token), ProcessAlive: r.ProcessAlive(state)}
}
func (r *unhealthyDetachedRuntime) ProcessAlive(vcsbroker.ServiceState) bool {
	return syscall.Kill(r.pid, 0) == nil
}
func (r *unhealthyDetachedRuntime) RequestTunnelRepair(vcsbroker.ServiceState) error {
	return errors.New("unexpected tunnel repair")
}
func (r *unhealthyDetachedRuntime) RequestRestart(cfg vcsBrokerServiceConfig, state vcsbroker.ServiceState) error {
	if err := writePrivateJSON(state.RequestPath, cfg); err != nil {
		return err
	}
	return syscall.Kill(r.pid, syscall.SIGUSR2)
}
func (r *unhealthyDetachedRuntime) RequestShutdown(vcsbroker.ServiceState) error { return nil }
func (r *unhealthyDetachedRuntime) Start(vcsBrokerServiceConfig) (vcsbroker.ServiceState, error) {
	return vcsbroker.ServiceState{}, errors.New("unhealthy live daemon must not be synchronously replaced")
}
func (r *unhealthyDetachedRuntime) Stop(vm.Backend, string, vcsbroker.ServiceState) error { return nil }
func (r *unhealthyDetachedRuntime) ForceStop(vm.Backend, string, vcsbroker.ServiceState) error {
	r.forceStop.Store(true)
	return errors.New("unhealthy live daemon was force-stopped")
}

func (r *detachedProbeRuntime) Inspect(_ vm.Backend, _ string, state vcsbroker.ServiceState) vcsBrokerHealth {
	status := vcsbroker.ProbeHost(state.HostPort, state.Token)
	return vcsBrokerHealth{Host: status, Tunnel: true, ProcessAlive: status != vcsbroker.HostProbeDead}
}
func (r *detachedProbeRuntime) RequestTunnelRepair(vcsbroker.ServiceState) error {
	return errors.New("unexpected tunnel repair")
}
func (r *detachedProbeRuntime) RequestRestart(vcsBrokerServiceConfig, vcsbroker.ServiceState) error {
	return errors.New("unexpected restart")
}
func (r *detachedProbeRuntime) RequestShutdown(vcsbroker.ServiceState) error { return nil }
func (r *detachedProbeRuntime) Start(cfg vcsBrokerServiceConfig) (vcsbroker.ServiceState, error) {
	r.mu.Lock()
	r.starts++
	generation := r.starts
	r.mu.Unlock()
	portPath := fmt.Sprintf("%s.probe-%d", cfg.ReadyPath, generation)
	process := exec.Command(os.Args[0], "-test.run", "^TestVCSBrokerSubprocessHelper$")
	process.Env = append(os.Environ(), "CLOISTER_VCS_HELPER=managed-probe-daemon", fmt.Sprintf("CLOISTER_VCS_GENERATION=%d", generation), "CLOISTER_VCS_PORT="+portPath)
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := process.Start(); err != nil {
		return vcsbroker.ServiceState{}, err
	}
	r.mu.Lock()
	r.processes = append(r.processes, process)
	r.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	port := 0
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(portPath)
		if err == nil {
			port, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if port > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if port == 0 {
		_ = process.Process.Kill()
		return vcsbroker.ServiceState{}, errors.New("detached probe daemon did not become ready")
	}
	state := vcsbroker.ServiceState{OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, BrokerPID: process.Process.Pid, TunnelPID: process.Process.Pid + 100000, HostPort: port, GuestPort: vcsBrokerGuestPort, Token: fmt.Sprintf("managed-probe-token-%d", generation), ConfigHash: cfg.ConfigHash, BuildID: cfg.BuildID, TunnelTarget: "vm.test", StatePath: cfg.StatePath, ConfigPath: cfg.ConfigPath, ReadyPath: cfg.ReadyPath, RepairPath: cfg.RepairPath, DrainPath: cfg.DrainPath, ActivityPath: cfg.ActivityPath, TransitionPath: cfg.TransitionPath, RequestPath: cfg.RequestPath, SpoolDir: cfg.SpoolDir, LogPath: cfg.LogPath}
	if err := vcsbroker.WriteServiceState(cfg.StatePath, state); err != nil {
		return vcsbroker.ServiceState{}, err
	}
	return vcsbroker.ReadServiceState(cfg.StatePath)
}
func (r *detachedProbeRuntime) Stop(_ vm.Backend, _ string, state vcsbroker.ServiceState) error {
	r.mu.Lock()
	r.stops++
	r.mu.Unlock()
	process, err := os.FindProcess(state.BrokerPID)
	if err == nil {
		_ = process.Kill()
	}
	return nil
}
func (r *detachedProbeRuntime) ForceStop(backend vm.Backend, profile string, state vcsbroker.ServiceState) error {
	return r.Stop(backend, profile, state)
}
func (r *detachedProbeRuntime) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, process := range r.processes {
		_ = process.Process.Kill()
		_ = process.Wait()
	}
}

func (r drainTimeoutRuntime) Stop(backend vm.Backend, profile string, state vcsbroker.ServiceState) error {
	if err := r.fakePersistentVCSRuntime.Stop(backend, profile, state); err != nil {
		return err
	}
	return formatVCSBrokerDrainError([]vcsbroker.ActiveCommand{{
		Tool: "git", Args: []string{"push", "origin", "main"}, Project: "/home/guest/workspaces/project",
	}})
}

func (r *blockingStopRuntime) Stop(backend vm.Backend, profile string, state vcsbroker.ServiceState) error {
	close(r.started)
	<-r.release
	return r.fakePersistentVCSRuntime.Stop(backend, profile, state)
}

func (m mismatchedPublishRuntime) Start(cfg vcsBrokerServiceConfig) (vcsbroker.ServiceState, error) {
	state, err := m.fakePersistentVCSRuntime.Start(cfg)
	if err != nil {
		return state, err
	}
	recorded := state
	recorded.Token = "different-token"
	if err := vcsbroker.WriteServiceState(cfg.StatePath, recorded); err != nil {
		return state, err
	}
	return state, nil
}

func newFakePersistentVCSRuntime() *fakePersistentVCSRuntime {
	return &fakePersistentVCSRuntime{brokerAlive: true, hostHealthy: true, tunnelAlive: true, endpointHealthy: true}
}

func (f *fakePersistentVCSRuntime) Inspect(_ vm.Backend, _ string, state vcsbroker.ServiceState) vcsBrokerHealth {
	f.mu.Lock()
	defer f.mu.Unlock()
	host := vcsbroker.HostProbeDead
	if f.brokerAlive && f.hostHealthy {
		host = vcsbroker.HostProbeHealthy
	}
	if f.brokerAlive && state.Phase == "stopping" {
		host = vcsbroker.HostProbeDraining
	}
	return vcsBrokerHealth{Host: host, Tunnel: f.brokerAlive && f.tunnelAlive && f.endpointHealthy, ProcessAlive: f.brokerAlive}
}

func (f *fakePersistentVCSRuntime) RequestTunnelRepair(state vcsbroker.ServiceState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repairs++
	f.tunnelAlive = true
	f.endpointHealthy = true
	state.TunnelPID += 100
	if err := vcsbroker.WriteServiceState(state.StatePath, state); err != nil {
		return err
	}
	return nil
}

func (f *fakePersistentVCSRuntime) RequestRestart(cfg vcsBrokerServiceConfig, state vcsbroker.ServiceState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	return writePrivateJSON(state.RequestPath, cfg)
}

func (f *fakePersistentVCSRuntime) RequestShutdown(vcsbroker.ServiceState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdowns++
	return nil
}

func (f *fakePersistentVCSRuntime) Start(cfg vcsBrokerServiceConfig) (vcsbroker.ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.brokerAlive = true
	f.hostHealthy = true
	f.tunnelAlive = true
	f.endpointHealthy = true
	state := vcsbroker.ServiceState{
		OwnerID: cfg.OwnerID, GenerationID: cfg.GenerationID, BrokerPID: 1000 + f.starts,
		TunnelPID: 2000 + f.starts, HostPort: 30000 + f.starts,
		GuestPort: vcsBrokerGuestPort, Token: fmt.Sprintf("token-%d", f.starts),
		ConfigHash: cfg.ConfigHash, BuildID: cfg.BuildID, TunnelTarget: "vm.test",
		StatePath: cfg.StatePath, ConfigPath: cfg.ConfigPath, ReadyPath: cfg.ReadyPath,
		RepairPath: cfg.RepairPath, DrainPath: cfg.DrainPath, ActivityPath: cfg.ActivityPath,
		TransitionPath: cfg.TransitionPath, RequestPath: cfg.RequestPath, SpoolDir: cfg.SpoolDir, LogPath: cfg.LogPath,
	}
	// The standalone service, not the ensure caller, publishes its token and
	// process identity.
	if err := vcsbroker.WriteServiceState(cfg.StatePath, state); err != nil {
		return vcsbroker.ServiceState{}, err
	}
	return vcsbroker.ReadServiceState(cfg.StatePath)
}

func (f *fakePersistentVCSRuntime) ProcessAlive(vcsbroker.ServiceState) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.brokerAlive
}

func (f *fakePersistentVCSRuntime) Stop(_ vm.Backend, _ string, state vcsbroker.ServiceState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, state)
	f.brokerAlive = false
	f.tunnelAlive = false
	f.endpointHealthy = false
	return nil
}

func (f *fakePersistentVCSRuntime) ForceStop(backend vm.Backend, profile string, state vcsbroker.ServiceState) error {
	return f.Stop(backend, profile, state)
}

func (f *fakePersistentVCSRuntime) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, len(f.stops)
}

func (f *fakePersistentVCSRuntime) repairCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repairs
}

func (f *fakePersistentVCSRuntime) restartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts
}

func newPersistentVCSTest(t *testing.T) (*vcsBrokerManager, *fakePersistentVCSRuntime, *config.Profile) {
	t.Helper()
	runtime := newFakePersistentVCSRuntime()
	var sequence atomic.Int64
	manager := &vcsBrokerManager{
		stateDir: t.TempDir(), lockWait: 500 * time.Millisecond, runtime: runtime,
		newID:   func() (string, error) { return fmt.Sprintf("request-%d", sequence.Add(1)), nil },
		buildID: "test-build",
	}
	profile := &config.Profile{
		Backend: "colima", StartDir: t.TempDir(),
		Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeBroker},
	}
	return manager, runtime, profile
}

func vcsTestBackend() vm.Backend {
	return &vm.MockBackend{SSHScriptOut: "__CLH[/home/guest]CLH__"}
}

func readVCSServiceState(t *testing.T, manager *vcsBrokerManager, profile string) vcsbroker.ServiceState {
	t.Helper()
	store := vcsbroker.NewStateStore(manager.stateDir, profile, manager.lockWait)
	locked, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	state, err := locked.Load()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestVCSBrokerRepeatedAndConcurrentEnsuresKeepOneOwnerAndToken(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- manager.ensure(vcsTestBackend(), "shared", "colima", profile)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	before := readVCSServiceState(t, manager, "shared")
	if err := manager.ensure(vcsTestBackend(), "shared", "colima", profile); err != nil {
		t.Fatal(err)
	}
	after := readVCSServiceState(t, manager, "shared")
	if starts, stops := runtime.counts(); starts != 1 || stops != 0 {
		t.Fatalf("starts=%d stops=%d, want one service and no displacement", starts, stops)
	}
	if before != after || after.Token != "token-1" {
		t.Fatalf("healthy ensure changed service or token: before=%#v after=%#v", before, after)
	}
}

func TestVCSBrokerSurvivesAllSessionExits(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "headless", "colima", profile); err != nil {
		t.Fatal(err)
	}
	// There is intentionally no session handle or close hook. Losing every
	// caller leaves the standalone service unchanged for headless guest work.
	state := readVCSServiceState(t, manager, "headless")
	if starts, stops := runtime.counts(); starts != 1 || stops != 0 || state.Token == "" {
		t.Fatalf("standalone service did not survive caller exit: starts=%d stops=%d state=%#v", starts, stops, state)
	}
}

func TestVCSBrokerModeChangeRequestsNonblockingDaemonRetirement(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "retired", "colima", profile); err != nil {
		t.Fatal(err)
	}
	profile.Workspace.Mode = config.WorkspaceModeVirtiofs
	started := time.Now()
	if err := manager.retire(vcsTestBackend(), "retired"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("retirement request waited for daemon drain: %s", elapsed)
	}
	state := readVCSServiceState(t, manager, "retired")
	runtime.mu.Lock()
	shutdowns := runtime.shutdowns
	runtime.mu.Unlock()
	if shutdowns != 1 || state.Phase != "retiring" {
		t.Fatalf("retirement requests=%d state=%#v", shutdowns, state)
	}
}

func TestVCSBrokerDeadProcessIsRestoredOnEnsure(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "restart", "colima", profile); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.brokerAlive = false
	runtime.mu.Unlock()
	if err := manager.ensure(vcsTestBackend(), "restart", "colima", profile); err != nil {
		t.Fatal(err)
	}
	state := readVCSServiceState(t, manager, "restart")
	if starts, stops := runtime.counts(); starts != 2 || stops != 1 || state.Token != "token-2" {
		t.Fatalf("dead process recovery starts=%d stops=%d state=%#v", starts, stops, state)
	}
}

func TestVCSBrokerFailedHostProbeRequestsGracefulReplacementWithoutForceStop(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "unhealthy", "colima", profile); err != nil {
		t.Fatal(err)
	}
	state := readVCSServiceState(t, manager, "unhealthy")
	activity := vcsBrokerActivity{OwnerID: state.OwnerID, GenerationID: state.GenerationID, Revision: 2, Commands: []vcsbroker.ActiveCommand{{
		Tool: "git", Args: []string{"commit", "-m", "change"}, Project: "/home/guest/workspaces/project",
	}}}
	if err := writePrivateJSON(state.ActivityPath, activity); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.hostHealthy = false
	runtime.mu.Unlock()
	started := time.Now()
	err := manager.ensure(vcsTestBackend(), "unhealthy", "colima", profile)
	if err == nil || !strings.Contains(err.Error(), "graceful replacement requested") || !strings.Contains(err.Error(), "git commit") {
		t.Fatalf("unhealthy ensure warning = %v", err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("unhealthy ensure waited for the daemon drain")
	}
	if starts, stops := runtime.counts(); starts != 1 || stops != 0 || runtime.restartCount() != 1 {
		t.Fatalf("unhealthy ensure starts=%d force-stops=%d transitions=%d", starts, stops, runtime.restartCount())
	}
	recorded := readVCSServiceState(t, manager, "unhealthy")
	if recorded.OwnerID != state.OwnerID || recorded.Phase != "unhealthy-replacement-pending" {
		t.Fatalf("unhealthy service ownership was displaced: %#v", recorded)
	}
	desired, err := readVCSBrokerServiceConfig(recorded.RequestPath, recorded.OwnerID)
	if err != nil || !desired.RestartDaemon || desired.GenerationID == recorded.GenerationID {
		t.Fatalf("unhealthy replacement config=%#v error=%v", desired, err)
	}
}

func TestVCSBrokerEnsureReplacesKilledDetachedDaemon(t *testing.T) {
	if os.Getenv("CLOISTER_VCS_HELPER") != "" {
		return
	}
	runtime := &detachedProbeRuntime{}
	t.Cleanup(runtime.cleanup)
	var sequence atomic.Int64
	manager := &vcsBrokerManager{stateDir: t.TempDir(), lockWait: time.Second, runtime: runtime, newID: func() (string, error) { return fmt.Sprintf("detached-owner-%d", sequence.Add(1)), nil }, buildID: "test-build"}
	profile := &config.Profile{Backend: "colima", StartDir: t.TempDir(), Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}}
	backend := vcsTestBackend()
	if err := manager.ensure(backend, "example", "colima", profile); err != nil {
		t.Fatal(err)
	}
	before := readVCSServiceState(t, manager, "example")
	if status := vcsbroker.ProbeHost(before.HostPort, before.Token); status != vcsbroker.HostProbeHealthy {
		t.Fatalf("initial detached daemon status = %v", status)
	}
	if err := syscall.Kill(before.BrokerPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for vcsbroker.ProbeHost(before.HostPort, before.Token) != vcsbroker.HostProbeDead && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := manager.ensure(backend, "example", "colima", profile); err != nil {
		t.Fatal(err)
	}
	after := readVCSServiceState(t, manager, "example")
	if after.BrokerPID == before.BrokerPID || after.Token == before.Token || vcsbroker.ProbeHost(after.HostPort, after.Token) != vcsbroker.HostProbeHealthy {
		t.Fatalf("killed detached daemon was not replaced: before=%#v after=%#v", before, after)
	}
	runtime.mu.Lock()
	starts, stops := runtime.starts, runtime.stops
	runtime.mu.Unlock()
	if starts != 2 || stops != 1 {
		t.Fatalf("detached replacement starts=%d stops=%d", starts, stops)
	}
}

func TestVCSBrokerDeadTunnelEndpointIsRestoredOnEnsure(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "tunnel", "colima", profile); err != nil {
		t.Fatal(err)
	}
	before := readVCSServiceState(t, manager, "tunnel")
	runtime.mu.Lock()
	runtime.endpointHealthy = false
	runtime.mu.Unlock()
	if err := manager.ensure(vcsTestBackend(), "tunnel", "colima", profile); err == nil || !strings.Contains(err.Error(), "repair requested") {
		t.Fatalf("tunnel repair ensure error = %v", err)
	}
	if starts, stops := runtime.counts(); starts != 1 || stops != 0 || runtime.repairCount() != 1 {
		t.Fatalf("dead tunnel recovery starts=%d stops=%d repairs=%d, want 1, 0, 1", starts, stops, runtime.repairCount())
	}
	after := readVCSServiceState(t, manager, "tunnel")
	if after.BrokerPID != before.BrokerPID || after.Token != before.Token || after.TunnelPID == before.TunnelPID {
		t.Fatalf("tunnel repair replaced daemon identity: before=%#v after=%#v", before, after)
	}
}

func TestVCSBrokerTunnelRepairDoesNotPauseInflightCommand(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := broker.SessionSpec{Profile: "example", ProjectID: "project", Name: "project", HostRoot: repo, GuestRoot: "~/workspaces/project"}
	mapper, err := vcsbroker.NewMapper("/home/guest", []broker.SessionSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	runner := &blockingCommandRunner{started: make(chan struct{}), release: make(chan struct{})}
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(&broker.Mock{}, mapper, runner), "repair-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	previousStart, previousStop := startVCSBrokerTunnelFn, stopVCSBrokerTunnelFn
	previousDeploy, previousProbe := deployVCSBrokerGuestFn, probeVCSBrokerGuestFn
	repaired := make(chan struct{}, 1)
	startVCSBrokerTunnelFn = func(_, _, owner string, hostPort, guestPort int, _ vm.SSHAccess) (tunnel.ReverseForwardOwner, error) {
		return tunnel.ReverseForwardOwner{OwnerID: owner, PID: 222, HostPort: hostPort, GuestPort: guestPort, Target: "vm.test"}, nil
	}
	stopVCSBrokerTunnelFn = func(string, string, tunnel.ReverseForwardOwner) bool { return true }
	deployVCSBrokerGuestFn = func(vm.Backend, string, int, string, string) error {
		repaired <- struct{}{}
		return nil
	}
	probeVCSBrokerGuestFn = func(vm.Backend, string, int, string, string) bool { return true }
	t.Cleanup(func() {
		startVCSBrokerTunnelFn, stopVCSBrokerTunnelFn = previousStart, previousStop
		deployVCSBrokerGuestFn, probeVCSBrokerGuestFn = previousDeploy, previousProbe
	})

	form := url.Values{"tool": {"git"}, "cwd": {"/home/guest/workspaces/project"}, "arg": {"status"}}
	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/exec", server.Port()), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer repair-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	result := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			_, requestErr = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		result <- requestErr
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}
	service := &runningVCSBrokerService{
		state: vcsbroker.ServiceState{
			OwnerID: "repair-owner", GenerationID: "repair-generation", BrokerPID: 111, TunnelPID: 221, HostPort: server.Port(),
			GuestPort: vcsBrokerGuestPort, Token: "repair-token", TunnelTarget: "vm.test",
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		backend: &vm.MockBackend{}, server: server,
		tunnel: tunnel.ReverseForwardOwner{OwnerID: "repair-generation", PID: 221, HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Target: "vm.test"},
	}
	cfg := vcsBrokerServiceConfig{
		OwnerID: "repair-owner", GenerationID: "repair-generation", Profile: "example", StatePath: service.state.StatePath,
		RepairPath: filepath.Join(filepath.Dir(service.state.StatePath), "repair.json"), DrainWait: time.Second,
	}
	signals := make(chan os.Signal)
	ticks := make(chan time.Time)
	maintenance := make(chan time.Time)
	loopDone := make(chan error, 1)
	go func() { loopDone <- runVCSBrokerServiceLoop(service, cfg, signals, ticks, maintenance) }()
	signals <- syscall.SIGUSR1
	select {
	case <-repaired:
	case <-time.After(time.Second):
		t.Fatal("tunnel repair waited behind the in-flight command")
	}
	if server.IsDraining() || service.state.BrokerPID != 111 || service.state.TunnelPID != 222 {
		t.Fatalf("tunnel repair paused or replaced the daemon: draining=%v state=%#v", server.IsDraining(), service.state)
	}
	if status := vcsbroker.ProbeHost(server.Port(), "repair-token"); status != vcsbroker.HostProbeHealthy {
		t.Fatalf("host endpoint during pending tunnel repair = %v", status)
	}
	close(runner.release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight command did not complete after tunnel repair")
	}
	if server.IsDraining() || service.state.BrokerPID != 111 || service.state.TunnelPID != 222 {
		t.Fatalf("tunnel repair state: draining=%v state=%#v", server.IsDraining(), service.state)
	}
	signals <- syscall.SIGTERM
	select {
	case err := <-loopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("service loop did not stop")
	}
}

func TestVCSBrokerWorkspaceConfigHashChangeRestartsService(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "changed", "colima", profile); err != nil {
		t.Fatal(err)
	}
	before := readVCSServiceState(t, manager, "changed")
	profile.Workspace.Ignore = []string{"generated/"}
	started := time.Now()
	if err := manager.ensure(vcsTestBackend(), "changed", "colima", profile); err == nil || !strings.Contains(err.Error(), "transition is pending") {
		t.Fatalf("configuration transition warning = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("deferred config restart blocked ensure for %s", elapsed)
	}
	after := readVCSServiceState(t, manager, "changed")
	if starts, stops := runtime.counts(); starts != 1 || stops != 0 || runtime.restartCount() != 1 {
		t.Fatalf("config restart starts=%d stops=%d requests=%d, want 1, 0, 1", starts, stops, runtime.restartCount())
	}
	if before != after {
		t.Fatalf("ensure changed a service before its daemon-owned transition: before=%#v after=%#v", before, after)
	}
}

func TestVCSBrokerDeferredConfigReloadDrainsAcceptedCommand(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	currentSpec := broker.SessionSpec{
		Profile: "example", ProjectID: "project", Name: "project",
		HostRoot: repo, GuestRoot: "~/workspaces/project",
	}
	mapper, err := vcsbroker.NewMapper("/home/guest", []broker.SessionSpec{currentSpec})
	if err != nil {
		t.Fatal(err)
	}
	runner := &blockingCommandRunner{started: make(chan struct{}), release: make(chan struct{})}
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(&broker.Mock{}, mapper, runner), "reload-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	stateDir := t.TempDir()
	store := vcsbroker.NewStateStore(stateDir, "example", time.Second)
	currentWorkspace := config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}
	currentHash, err := vcsBrokerConfigHash("/home/guest", currentWorkspace, []broker.SessionSpec{currentSpec})
	if err != nil {
		t.Fatal(err)
	}
	state := vcsbroker.ServiceState{
		OwnerID: "reload-owner", GenerationID: "reload-generation", BrokerPID: os.Getpid(), TunnelPID: 200,
		HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: "reload-token",
		ConfigHash: currentHash, BuildID: "same-build", TunnelTarget: "vm.test",
		StatePath: store.StatePath, ConfigPath: filepath.Join(stateDir, "service.json"),
		ReadyPath: filepath.Join(stateDir, "ready.json"), RepairPath: filepath.Join(stateDir, "repair.json"),
		DrainPath: filepath.Join(stateDir, "drain.json"), RequestPath: filepath.Join(stateDir, "request.json"), LogPath: filepath.Join(stateDir, "service.log"),
	}
	if err := vcsbroker.WriteServiceState(state.StatePath, state); err != nil {
		t.Fatal(err)
	}
	current := vcsBrokerServiceConfig{
		OwnerID: state.OwnerID, GenerationID: state.GenerationID, Profile: "example", Backend: "colima", GuestHome: "/home/guest",
		Specs: []broker.SessionSpec{currentSpec}, Workspace: currentWorkspace, ConfigHash: currentHash,
		BuildID: state.BuildID, StatePath: state.StatePath, ConfigPath: state.ConfigPath,
		ReadyPath: state.ReadyPath, RepairPath: state.RepairPath, DrainPath: state.DrainPath,
		RequestPath: state.RequestPath, LogPath: state.LogPath, DrainWait: time.Second,
	}
	desired := current
	desired.Workspace.Ignore = []string{"generated/"}
	desired.ConfigHash, err = vcsBrokerConfigHash(desired.GuestHome, desired.Workspace, desired.Specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(current.RequestPath, desired); err != nil {
		t.Fatal(err)
	}

	previousBroker := newWorkspaceBroker
	previousDeploy := deployVCSBrokerGuestFn
	previousStop := stopVCSBrokerTunnelFn
	previousRunner := newVCSBrokerHostRunnerFn
	newWorkspaceBroker = func() (broker.SyncBroker, error) { return &broker.Mock{}, nil }
	newVCSBrokerHostRunnerFn = func() (vcsbroker.HostCommandRunner, error) { return nil, nil }
	deployed := make(chan string, 1)
	deployVCSBrokerGuestFn = func(_ vm.Backend, _ string, _ int, token, _ string) error {
		deployed <- token
		return nil
	}
	stopVCSBrokerTunnelFn = func(string, string, tunnel.ReverseForwardOwner) bool { return true }
	t.Cleanup(func() {
		newWorkspaceBroker = previousBroker
		deployVCSBrokerGuestFn = previousDeploy
		stopVCSBrokerTunnelFn = previousStop
		newVCSBrokerHostRunnerFn = previousRunner
	})

	service := &runningVCSBrokerService{
		state: state, backend: &vm.MockBackend{}, server: server,
		tunnel: tunnel.ReverseForwardOwner{OwnerID: state.OwnerID, PID: state.TunnelPID, HostPort: state.HostPort, GuestPort: state.GuestPort, Target: state.TunnelTarget},
	}
	signals := make(chan os.Signal)
	ticks := make(chan time.Time)
	maintenance := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- runVCSBrokerServiceLoop(service, current, signals, ticks, maintenance) }()

	form := url.Values{"tool": {"git"}, "cwd": {"/home/guest/workspaces/project"}, "arg": {"status"}}
	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/exec", server.Port()), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer reload-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	commandDone := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			_, requestErr = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		commandDone <- requestErr
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}
	signals <- syscall.SIGUSR2
	deadline := time.Now().Add(time.Second)
	for !server.IsDraining() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if status := vcsbroker.ProbeHost(server.Port(), "reload-token"); status != vcsbroker.HostProbeDraining {
		t.Fatalf("broker did not close admission while the active command drained: %v", status)
	}
	before, err := vcsbroker.ReadServiceState(state.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if before.ConfigHash != currentHash {
		t.Fatal("deferred reload applied while a command was active")
	}

	close(runner.release)
	select {
	case err := <-commandDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("active command did not complete")
	}
	select {
	case token := <-deployed:
		if token != "reload-token" {
			t.Fatalf("reload changed token to %q", token)
		}
	case <-time.After(time.Second):
		t.Fatal("drained reload did not refresh guest shims")
	}
	var after vcsbroker.ServiceState
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		after, err = vcsbroker.ReadServiceState(state.StatePath)
		if err == nil && after.ConfigHash == desired.ConfigHash {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if after.ConfigHash != desired.ConfigHash || after.Token != state.Token || after.BrokerPID != state.BrokerPID {
		t.Fatalf("drained reload state = %#v (read error %v)", after, err)
	}

	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("service loop did not stop")
	}
}

func TestVCSBrokerPureAdditionReloadsWithoutClosingAdmission(t *testing.T) {
	rootA, _ := filepath.EvalSymlinks(t.TempDir())
	rootB, _ := filepath.EvalSymlinks(t.TempDir())
	specA := broker.SessionSpec{Profile: "example", ProjectID: "a", Name: "a", HostRoot: rootA, GuestRoot: "~/workspaces/a"}
	specB := broker.SessionSpec{Profile: "example", ProjectID: "b", Name: "b", HostRoot: rootB, GuestRoot: "~/workspaces/b"}
	mapper, err := vcsbroker.NewMapper("/home/guest", []broker.SessionSpec{specA})
	if err != nil {
		t.Fatal(err)
	}
	runner := &blockingCommandRunner{started: make(chan struct{}), release: make(chan struct{})}
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(&broker.Mock{}, mapper, runner), "addition-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	var sawDrain atomic.Bool
	server.SetStatusObserver(func(status vcsbroker.ServerStatus) {
		if status.Draining {
			sawDrain.Store(true)
		}
	})
	stateDir := t.TempDir()
	store := vcsbroker.NewStateStore(stateDir, "example", time.Second)
	workspace := config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}
	currentHash, _ := vcsBrokerConfigHash("/home/guest", workspace, []broker.SessionSpec{specA})
	current := newVCSBrokerServiceConfig(stateDir, store.StatePath, "addition-owner", "addition-generation", "example", "colima", "/home/guest", workspace, []broker.SessionSpec{specA}, currentHash, "same-build")
	current.DrainWait = time.Second
	state := vcsbroker.ServiceState{
		OwnerID: current.OwnerID, GenerationID: current.GenerationID, BrokerPID: os.Getpid(), TunnelPID: 200, HostPort: server.Port(), GuestPort: vcsBrokerGuestPort,
		Token: "addition-token", ConfigHash: current.ConfigHash, BuildID: current.BuildID, TunnelTarget: "vm.test",
		StatePath: current.StatePath, ConfigPath: current.ConfigPath, ReadyPath: current.ReadyPath, RepairPath: current.RepairPath,
		DrainPath: current.DrainPath, ActivityPath: current.ActivityPath, TransitionPath: current.TransitionPath, RequestPath: current.RequestPath, SpoolDir: current.SpoolDir, LogPath: current.LogPath,
	}
	if err := vcsbroker.WriteServiceState(state.StatePath, state); err != nil {
		t.Fatal(err)
	}
	desired := current
	desired.Specs = []broker.SessionSpec{specA, specB}
	desired.ConfigHash, _ = vcsBrokerConfigHash(desired.GuestHome, desired.Workspace, desired.Specs)
	if !pureAdditiveVCSBrokerConfig(current, desired) {
		t.Fatal("strict project addition was not classified as additive")
	}
	changed := desired
	changed.Specs = append([]broker.SessionSpec(nil), desired.Specs...)
	changed.Specs[0].HostRoot = rootB
	if pureAdditiveVCSBrokerConfig(current, changed) {
		t.Fatal("changed existing project was classified as additive")
	}
	if err := writePrivateJSON(current.RequestPath, desired); err != nil {
		t.Fatal(err)
	}
	previousWorkspace, previousRunner, previousDeploy := newWorkspaceBroker, newVCSBrokerHostRunnerFn, deployVCSBrokerGuestFn
	newWorkspaceBroker = func() (broker.SyncBroker, error) { return &broker.Mock{}, nil }
	newVCSBrokerHostRunnerFn = func() (vcsbroker.HostCommandRunner, error) { return runner, nil }
	deployed := make(chan struct{}, 1)
	deployVCSBrokerGuestFn = func(vm.Backend, string, int, string, string) error { deployed <- struct{}{}; return nil }
	t.Cleanup(func() {
		newWorkspaceBroker, newVCSBrokerHostRunnerFn, deployVCSBrokerGuestFn = previousWorkspace, previousRunner, previousDeploy
	})
	service := &runningVCSBrokerService{state: state, backend: &vm.MockBackend{}, server: server, tunnel: tunnel.ReverseForwardOwner{OwnerID: state.GenerationID, PID: state.TunnelPID, HostPort: state.HostPort, GuestPort: state.GuestPort, Target: state.TunnelTarget}}
	signals := make(chan os.Signal, 2)
	done := make(chan error, 1)
	go func() {
		done <- runVCSBrokerServiceLoop(service, current, signals, make(chan time.Time), make(chan time.Time))
	}()
	form := url.Values{"tool": {"git"}, "cwd": {"/home/guest/workspaces/a"}, "arg": {"status"}}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/exec", server.Port()), strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer addition-token")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	requestDone := make(chan struct{})
	go func() {
		response, _ := http.DefaultClient.Do(req)
		if response != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		close(requestDone)
	}()
	<-runner.started
	signals <- syscall.SIGUSR2
	select {
	case <-deployed:
	case <-time.After(time.Second):
		t.Fatal("additive mapper was not deployed")
	}
	if sawDrain.Load() || server.IsDraining() {
		t.Fatal("pure addition closed command admission")
	}
	close(runner.release)
	<-requestDone
	signals <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVCSBrokerDeferredRestartTimeoutReopensCurrentService(t *testing.T) {
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	spec := broker.SessionSpec{Profile: "example", ProjectID: "project", Name: "project", HostRoot: repo, GuestRoot: "~/workspaces/project"}
	mapper, err := vcsbroker.NewMapper("/home/guest", []broker.SessionSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	runner := &blockingCommandRunner{started: make(chan struct{}), release: make(chan struct{})}
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(&broker.Mock{}, mapper, runner), "timeout-reload-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	stateDir := t.TempDir()
	store := vcsbroker.NewStateStore(stateDir, "example", time.Second)
	workspace := config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}
	hash, _ := vcsBrokerConfigHash("/home/guest", workspace, []broker.SessionSpec{spec})
	state := vcsbroker.ServiceState{OwnerID: "timeout-owner", GenerationID: "timeout-generation", BrokerPID: os.Getpid(), TunnelPID: 200, HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: "timeout-reload-token", ConfigHash: hash, BuildID: "same-build", TunnelTarget: "vm.test", StatePath: store.StatePath, ConfigPath: filepath.Join(stateDir, "service.json"), ReadyPath: filepath.Join(stateDir, "ready.json"), RepairPath: filepath.Join(stateDir, "repair.json"), DrainPath: filepath.Join(stateDir, "drain.json"), ActivityPath: filepath.Join(stateDir, "activity.json"), TransitionPath: filepath.Join(stateDir, "transition.json"), RequestPath: filepath.Join(stateDir, "request.json"), SpoolDir: filepath.Join(stateDir, "spools"), LogPath: filepath.Join(stateDir, "service.log")}
	if err := vcsbroker.WriteServiceState(state.StatePath, state); err != nil {
		t.Fatal(err)
	}
	current := vcsBrokerServiceConfig{OwnerID: state.OwnerID, GenerationID: state.GenerationID, Profile: "example", GuestHome: "/home/guest", Specs: []broker.SessionSpec{spec}, Workspace: workspace, ConfigHash: hash, BuildID: state.BuildID, StatePath: state.StatePath, ConfigPath: state.ConfigPath, ReadyPath: state.ReadyPath, RepairPath: state.RepairPath, DrainPath: state.DrainPath, ActivityPath: state.ActivityPath, TransitionPath: state.TransitionPath, RequestPath: state.RequestPath, SpoolDir: state.SpoolDir, LogPath: state.LogPath, DrainWait: 30 * time.Millisecond}
	desired := current
	desired.Workspace.Ignore = []string{"changed/"}
	desired.ConfigHash, _ = vcsBrokerConfigHash(desired.GuestHome, desired.Workspace, desired.Specs)
	if err := writePrivateJSON(current.RequestPath, desired); err != nil {
		t.Fatal(err)
	}
	service := &runningVCSBrokerService{state: state, backend: &vm.MockBackend{}, server: server, tunnel: tunnel.ReverseForwardOwner{OwnerID: state.GenerationID, PID: state.TunnelPID, HostPort: state.HostPort, GuestPort: state.GuestPort, Target: state.TunnelTarget}}
	signals := make(chan os.Signal)
	done := make(chan error, 1)
	maintenance := make(chan time.Time)
	previousRetryBase := vcsBrokerTransitionRetryBase
	vcsBrokerTransitionRetryBase = 10 * time.Millisecond
	previousWorkspace, previousRunner, previousDeploy := newWorkspaceBroker, newVCSBrokerHostRunnerFn, deployVCSBrokerGuestFn
	newWorkspaceBroker = func() (broker.SyncBroker, error) { return &broker.Mock{}, nil }
	newVCSBrokerHostRunnerFn = func() (vcsbroker.HostCommandRunner, error) { return nil, nil }
	deployVCSBrokerGuestFn = func(vm.Backend, string, int, string, string) error { return nil }
	t.Cleanup(func() {
		vcsBrokerTransitionRetryBase = previousRetryBase
		newWorkspaceBroker, newVCSBrokerHostRunnerFn, deployVCSBrokerGuestFn = previousWorkspace, previousRunner, previousDeploy
	})
	go func() {
		done <- runVCSBrokerServiceLoop(service, current, signals, make(chan time.Time), maintenance)
	}()
	form := url.Values{"tool": {"git"}, "cwd": {"/home/guest/workspaces/project"}, "arg": {"status"}}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/exec", server.Port()), strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer timeout-reload-token")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	requestDone := make(chan struct{})
	go func() {
		response, requestErr := http.DefaultClient.Do(req)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		close(requestDone)
	}()
	<-runner.started
	signals <- syscall.SIGUSR2
	deadline := time.Now().Add(time.Second)
	for !server.IsDraining() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !server.IsDraining() {
		t.Fatal("deferred restart did not close admission")
	}
	for (server.IsDraining() || vcsbroker.ProbeHost(server.Port(), "timeout-reload-token") != vcsbroker.HostProbeHealthy) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.IsDraining() || vcsbroker.ProbeHost(server.Port(), "timeout-reload-token") != vcsbroker.HostProbeHealthy {
		t.Fatal("timed-out deferred restart did not reopen the current broker")
	}
	currentState, err := vcsbroker.ReadServiceState(state.StatePath)
	if err != nil || currentState.ConfigHash != hash {
		t.Fatalf("timed-out restart removed or changed current state: %#v, %v", currentState, err)
	}
	transitionStatus := vcsBrokerTransitionStatus{}
	data, err := os.ReadFile(state.TransitionPath)
	if err != nil || json.Unmarshal(data, &transitionStatus) != nil || transitionStatus.State != "retry-pending" {
		t.Fatalf("timed-out transition was not retained: %s error=%v", data, err)
	}
	close(runner.release)
	<-requestDone
	time.Sleep(15 * time.Millisecond)
	maintenance <- time.Now()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		currentState, _ = vcsbroker.ReadServiceState(state.StatePath)
		if currentState.ConfigHash == desired.ConfigHash {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if currentState.ConfigHash != desired.ConfigHash {
		t.Fatalf("retained transition did not retry successfully: %#v", currentState)
	}
	signals <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVCSBrokerTransitionStopsRetryingAfterBoundAndExplicitEnsureResumes(t *testing.T) {
	rootA, _ := filepath.EvalSymlinks(t.TempDir())
	rootB, _ := filepath.EvalSymlinks(t.TempDir())
	specA := broker.SessionSpec{Profile: "example", ProjectID: "a", Name: "a", HostRoot: rootA, GuestRoot: "~/workspaces/a"}
	specB := broker.SessionSpec{Profile: "example", ProjectID: "b", Name: "b", HostRoot: rootB, GuestRoot: "~/workspaces/b"}
	mapper, err := vcsbroker.NewMapper("/home/guest", []broker.SessionSpec{specA})
	if err != nil {
		t.Fatal(err)
	}
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(&broker.Mock{}, mapper, nil), "bounded-retry-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	stateDir := t.TempDir()
	store := vcsbroker.NewStateStore(stateDir, "example", time.Second)
	workspace := config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}
	hash, _ := vcsBrokerConfigHash("/home/guest", workspace, []broker.SessionSpec{specA})
	current := newVCSBrokerServiceConfig(stateDir, store.StatePath, "bounded-owner", "bounded-generation", "example", "colima", "/home/guest", workspace, []broker.SessionSpec{specA}, hash, "same-build")
	current.DrainWait = time.Second
	desired := current
	desired.Specs = []broker.SessionSpec{specA, specB}
	desired.ConfigHash, _ = vcsBrokerConfigHash(desired.GuestHome, desired.Workspace, desired.Specs)
	if err := writePrivateJSON(current.RequestPath, desired); err != nil {
		t.Fatal(err)
	}
	state := vcsbroker.ServiceState{
		OwnerID: current.OwnerID, GenerationID: current.GenerationID, BrokerPID: os.Getpid(),
		TunnelPID: 200, HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: "bounded-retry-token",
		ConfigHash: current.ConfigHash, BuildID: current.BuildID, TunnelTarget: "vm.test",
		StatePath: current.StatePath, ConfigPath: current.ConfigPath, ReadyPath: current.ReadyPath,
		RepairPath: current.RepairPath, DrainPath: current.DrainPath, ActivityPath: current.ActivityPath,
		TransitionPath: current.TransitionPath, RequestPath: current.RequestPath, SpoolDir: current.SpoolDir, LogPath: current.LogPath,
	}
	service := &runningVCSBrokerService{state: state, backend: &vm.MockBackend{}, server: server}
	previousWorkspace, previousRunner, previousDeploy := newWorkspaceBroker, newVCSBrokerHostRunnerFn, deployVCSBrokerGuestFn
	previousRetryBase := vcsBrokerTransitionRetryBase
	newWorkspaceBroker = func() (broker.SyncBroker, error) { return &broker.Mock{}, nil }
	newVCSBrokerHostRunnerFn = func() (vcsbroker.HostCommandRunner, error) { return nil, nil }
	var deploys atomic.Int64
	deployVCSBrokerGuestFn = func(vm.Backend, string, int, string, string) error {
		deploys.Add(1)
		return errors.New("guest update failed")
	}
	vcsBrokerTransitionRetryBase = time.Millisecond
	t.Cleanup(func() {
		newWorkspaceBroker, newVCSBrokerHostRunnerFn, deployVCSBrokerGuestFn = previousWorkspace, previousRunner, previousDeploy
		vcsBrokerTransitionRetryBase = previousRetryBase
	})
	signals := make(chan os.Signal, 2)
	maintenance := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- runVCSBrokerServiceLoop(service, current, signals, make(chan time.Time), maintenance) }()
	signals <- syscall.SIGUSR2
	for attempt := 2; attempt <= vcsBrokerMaxTransitionAttempts; attempt++ {
		deadline := time.Now().Add(time.Second)
		for deploys.Load() < int64(attempt-1) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(vcsBrokerTransitionRetryDelay(attempt-1) + time.Millisecond)
		maintenance <- time.Now()
	}
	var status vcsBrokerTransitionStatus
	deadline := time.Now().Add(time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(current.TransitionPath)
		if json.Unmarshal(data, &status) == nil && status.State == "failed" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	data, err = os.ReadFile(current.TransitionPath)
	status = vcsBrokerTransitionStatus{}
	if err != nil || json.Unmarshal(data, &status) != nil || status.State != "failed" || status.Attempt != vcsBrokerMaxTransitionAttempts {
		t.Fatalf("bounded transition status=%#v data=%s error=%v", status, data, err)
	}
	maintenance <- time.Now().Add(time.Hour)
	time.Sleep(10 * time.Millisecond)
	if got := deploys.Load(); got != vcsBrokerMaxTransitionAttempts {
		t.Fatalf("failed transition retried automatically: attempts=%d", got)
	}
	signals <- syscall.SIGUSR2
	deadline = time.Now().Add(time.Second)
	for deploys.Load() != vcsBrokerMaxTransitionAttempts+1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := deploys.Load(); got != vcsBrokerMaxTransitionAttempts+1 {
		t.Fatalf("explicit ensure did not resume failed transition: attempts=%d", got)
	}
	signals <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVCSBrokerConfigHashIncludesWorkspaceMode(t *testing.T) {
	brokerHash, err := vcsBrokerConfigHash("/home/guest", config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}, nil)
	if err != nil {
		t.Fatal(err)
	}
	workspaceHash, err := vcsBrokerConfigHash("/home/guest", config.WorkspaceConfig{Mode: config.WorkspaceModeWorkspace}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if brokerHash == workspaceHash {
		t.Fatal("workspace mode change did not change the VCS broker configuration hash")
	}
}

func TestVCSBrokerBuildIdentityMismatchRequestsDeferredReplacement(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "upgrade", "colima", profile); err != nil {
		t.Fatal(err)
	}
	before := readVCSServiceState(t, manager, "upgrade")
	manager.buildID = "replacement-build"
	if err := manager.ensure(vcsTestBackend(), "upgrade", "colima", profile); err == nil || !strings.Contains(err.Error(), "transition is pending") {
		t.Fatalf("build transition warning = %v", err)
	}
	after := readVCSServiceState(t, manager, "upgrade")
	if starts, stops := runtime.counts(); starts != 1 || stops != 0 || runtime.restartCount() != 1 {
		t.Fatalf("build replacement starts=%d stops=%d requests=%d, want 1, 0, 1", starts, stops, runtime.restartCount())
	}
	if before != after {
		t.Fatalf("ensure replaced the daemon before its daemon-owned drain: before=%#v after=%#v", before, after)
	}
}

func TestVCSBrokerDaemonPerformsBuildReplacementOnlyAfterDrain(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := broker.SessionSpec{
		Profile: "example", ProjectID: "project", Name: "project",
		HostRoot: repo, GuestRoot: "~/workspaces/project",
	}
	mapper, err := vcsbroker.NewMapper("/home/guest", []broker.SessionSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(&broker.Mock{}, mapper, nil), "old-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	stateDir := t.TempDir()
	store := vcsbroker.NewStateStore(stateDir, "example", time.Second)
	workspace := config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}
	hash, err := vcsBrokerConfigHash("/home/guest", workspace, []broker.SessionSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	state := vcsbroker.ServiceState{
		OwnerID: "build-owner", GenerationID: "build-old-generation", BrokerPID: os.Getpid(), TunnelPID: 201,
		HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: "old-token",
		ConfigHash: hash, BuildID: "old-build", TunnelTarget: "vm.test",
		StatePath: store.StatePath, ConfigPath: filepath.Join(stateDir, "service.json"),
		ReadyPath: filepath.Join(stateDir, "ready.json"), RepairPath: filepath.Join(stateDir, "repair.json"),
		DrainPath: filepath.Join(stateDir, "drain.json"), RequestPath: filepath.Join(stateDir, "request.json"), LogPath: filepath.Join(stateDir, "service.log"),
	}
	if err := vcsbroker.WriteServiceState(state.StatePath, state); err != nil {
		t.Fatal(err)
	}
	current := vcsBrokerServiceConfig{
		OwnerID: state.OwnerID, GenerationID: state.GenerationID, Profile: "example", Backend: "colima", GuestHome: "/home/guest",
		Specs: []broker.SessionSpec{spec}, Workspace: workspace, ConfigHash: hash, BuildID: state.BuildID,
		StatePath: state.StatePath, ConfigPath: state.ConfigPath, ReadyPath: state.ReadyPath,
		RepairPath: state.RepairPath, DrainPath: state.DrainPath, RequestPath: state.RequestPath, LogPath: state.LogPath, DrainWait: time.Second,
	}
	desired := current
	desired.BuildID = "new-build"
	desired = replacementVCSBrokerServiceConfig(stateDir, desired, "build-new-generation")
	service := &runningVCSBrokerService{
		state: state, backend: &vm.MockBackend{}, server: server,
		tunnel: tunnel.ReverseForwardOwner{OwnerID: state.GenerationID, PID: state.TunnelPID, HostPort: state.HostPort, GuestPort: state.GuestPort, Target: state.TunnelTarget},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := server.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	previousStart := startVCSBrokerReplacementFn
	previousStop := stopVCSBrokerTunnelFn
	started := 0
	var events []string
	startVCSBrokerReplacementFn = func(cfg vcsBrokerServiceConfig) (vcsbroker.ServiceState, error) {
		events = append(events, "start-replacement")
		started++
		replacement := state
		replacement.BrokerPID++
		replacement.TunnelPID++
		replacement.Token = "new-token"
		replacement.BuildID = cfg.BuildID
		replacement.GenerationID = cfg.GenerationID
		replacement.ConfigPath = cfg.ConfigPath
		replacement.ReadyPath = cfg.ReadyPath
		replacement.RepairPath = cfg.RepairPath
		replacement.DrainPath = cfg.DrainPath
		replacement.ActivityPath = cfg.ActivityPath
		replacement.TransitionPath = cfg.TransitionPath
		replacement.RequestPath = cfg.RequestPath
		replacement.SpoolDir = cfg.SpoolDir
		replacement.LogPath = cfg.LogPath
		if err := vcsbroker.WriteServiceState(cfg.StatePath, replacement); err != nil {
			return vcsbroker.ServiceState{}, err
		}
		return replacement, nil
	}
	stopVCSBrokerTunnelFn = func(string, string, tunnel.ReverseForwardOwner) bool {
		events = append(events, "stop-old-tunnel")
		return true
	}
	t.Cleanup(func() {
		startVCSBrokerReplacementFn = previousStart
		stopVCSBrokerTunnelFn = previousStop
	})
	replaced, err := service.applyDesiredConfig(current, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !replaced || started != 1 {
		t.Fatalf("build replacement applied=%v starts=%d", replaced, started)
	}
	if got := strings.Join(events, ","); got != "stop-old-tunnel,start-replacement" {
		t.Fatalf("replacement ordering = %s", got)
	}
}

func TestVCSBrokerStopTearsDownPersistentService(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	backend := vcsTestBackend()
	if err := manager.ensure(backend, "stopped", "colima", profile); err != nil {
		t.Fatal(err)
	}
	if err := manager.stop(backend, "stopped"); err != nil {
		t.Fatal(err)
	}
	state := readVCSServiceState(t, manager, "stopped")
	if starts, stops := runtime.counts(); starts != 1 || stops != 1 || state.OwnerID != "" {
		t.Fatalf("stop lifecycle starts=%d stops=%d state=%#v", starts, stops, state)
	}
	statePath := vcsbroker.NewStateStore(manager.stateDir, "stopped", manager.lockWait).StatePath
	if _, err := os.Stat(statePath); !os.IsNotExist(err) || state.Token != "" {
		t.Fatalf("stop left a state token: state=%#v stat error=%v", state, err)
	}
}

func TestVCSBrokerPrefixProfileNamesRemainIsolated(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	backend := vcsTestBackend()
	for _, name := range []string{"team", "team-long"} {
		if err := manager.ensure(backend, name, "colima", profile); err != nil {
			t.Fatal(err)
		}
	}
	first := readVCSServiceState(t, manager, "team")
	second := readVCSServiceState(t, manager, "team-long")
	if first.OwnerID == "" || second.OwnerID == "" || first.OwnerID == second.OwnerID || first.StatePath == second.StatePath {
		t.Fatalf("prefix profile ownership collided: first=%#v second=%#v", first, second)
	}
	if err := manager.stop(backend, "team"); err != nil {
		t.Fatal(err)
	}
	after := readVCSServiceState(t, manager, "team-long")
	if after != second {
		t.Fatalf("stopping prefix profile changed peer: before=%#v after=%#v", second, after)
	}
	if starts, stops := runtime.counts(); starts != 2 || stops != 1 {
		t.Fatalf("prefix profile lifecycle starts=%d stops=%d", starts, stops)
	}
}

func TestVCSBrokerStopProgressNamesCommandProjectAndWait(t *testing.T) {
	var output strings.Builder
	writeVCSBrokerStopProgress(&output, 12*time.Second, []vcsbroker.ActiveCommand{{
		Tool: "git", Args: []string{"push", "origin", "main"}, Project: "/home/guest/workspaces/project",
	}})
	for _, want := range []string{"Waiting 12s", "git push origin main", "/home/guest/workspaces/project"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("stop progress %q missing %q", output.String(), want)
		}
	}
}

func TestVCSBrokerDrainTimeoutStopsExplicitlyAndReportsInterruptedCommand(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "timeout", "colima", profile); err != nil {
		t.Fatal(err)
	}
	manager.runtime = drainTimeoutRuntime{runtime}
	err := manager.stop(vcsTestBackend(), "timeout")
	if err == nil || !strings.Contains(err.Error(), `"git push origin main"`) || !strings.Contains(err.Error(), "/home/guest/workspaces/project") {
		t.Fatalf("drain timeout error = %v", err)
	}
	state := readVCSServiceState(t, manager, "timeout")
	if state.OwnerID != "" {
		t.Fatalf("explicit timed-out stop left a live service record: %#v", state)
	}
}

func TestVMLifecycleContinuesAfterBrokerDrainTimeoutWarning(t *testing.T) {
	previous := stopVCSBrokerFn
	stopVCSBrokerFn = func(vm.Backend, string) error {
		return formatVCSBrokerDrainError([]vcsbroker.ActiveCommand{{
			Tool: "git", Args: []string{"push"}, Project: "/home/guest/workspaces/project",
		}})
	}
	t.Cleanup(func() { stopVCSBrokerFn = previous })
	backend := &vm.MockBackend{}
	stderr := captureStderr(t, func() {
		if err := stopVM(backend, "example", &config.Profile{}, false, false); err != nil {
			t.Fatal(err)
		}
	})
	if len(backend.StopCalls) != 1 || !strings.Contains(stderr, `"git push"`) || !strings.Contains(stderr, "continuing VM lifecycle teardown") {
		t.Fatalf("VM stops=%v warning=%q", backend.StopCalls, stderr)
	}
}

func TestVCSBrokerLongDrainDoesNotHoldProfileLock(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "draining", "colima", profile); err != nil {
		t.Fatal(err)
	}
	blocking := &blockingStopRuntime{fakePersistentVCSRuntime: runtime, started: make(chan struct{}), release: make(chan struct{})}
	manager.runtime = blocking
	stopDone := make(chan error, 1)
	go func() { stopDone <- manager.stop(vcsTestBackend(), "draining") }()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("stop did not begin its drain")
	}
	started := time.Now()
	err := manager.ensure(vcsTestBackend(), "draining", "colima", profile)
	if err == nil || !strings.Contains(err.Error(), "is stopping") {
		t.Fatalf("concurrent ensure error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("concurrent ensure waited behind drain for %s", elapsed)
	}
	close(blocking.release)
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
}

func TestVCSBrokerRejectsMismatchedStatePublishedByService(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	manager.runtime = mismatchedPublishRuntime{runtime}
	err := manager.ensure(vcsTestBackend(), "mismatch", "colima", profile)
	if err == nil || !strings.Contains(err.Error(), "mismatched ownership state") {
		t.Fatalf("ensure error = %v, want ownership mismatch", err)
	}
	if starts, stops := runtime.counts(); starts != 1 || stops != 1 {
		t.Fatalf("mismatched service starts=%d stops=%d, want 1 and 1", starts, stops)
	}
}

func TestVCSBrokerLockWaitIsBounded(t *testing.T) {
	manager, _, profile := newPersistentVCSTest(t)
	manager.lockWait = 75 * time.Millisecond
	store := vcsbroker.NewStateStore(manager.stateDir, "blocked", time.Second)
	locked, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	started := time.Now()
	err = manager.ensure(vcsTestBackend(), "blocked", "colima", profile)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("ensure error = %v, want lock timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded lock wait took %s", elapsed)
	}
}

func TestVCSBrokerProductionLockWaitReturnsAfterTwelveSeconds(t *testing.T) {
	if vcsBrokerLockWait != 12*time.Second {
		t.Fatalf("production lock bound = %s, want 12s", vcsBrokerLockWait)
	}
	manager, _, profile := newPersistentVCSTest(t)
	manager.lockWait = vcsBrokerLockWait
	store := vcsbroker.NewStateStore(manager.stateDir, "production-bound", time.Second)
	locked, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	started := time.Now()
	err = manager.ensure(vcsTestBackend(), "production-bound", "colima", profile)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "timed out after 12s") {
		t.Fatalf("ensure error = %v", err)
	}
	if elapsed < 12*time.Second || elapsed > 13*time.Second {
		t.Fatalf("production lock wait = %s, want 12s..13s", elapsed)
	}
}

func TestVCSBrokerReusedUnrelatedPIDIsNeverKilled(t *testing.T) {
	process := exec.Command("sleep", "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	state := vcsbroker.ServiceState{
		OwnerID: "stale-owner", GenerationID: "stale-generation", BrokerPID: process.Process.Pid, TunnelPID: process.Process.Pid,
		HostPort: 34567, GuestPort: vcsBrokerGuestPort, Token: "stale-token",
		ConfigHash: "stale-hash", BuildID: "stale-build", TunnelTarget: "vm.test",
		StatePath: t.TempDir() + "/state", ConfigPath: t.TempDir() + "/config", ReadyPath: t.TempDir() + "/ready",
		RepairPath: t.TempDir() + "/repair", DrainPath: t.TempDir() + "/drain", LogPath: t.TempDir() + "/log",
	}
	backend := &vm.MockBackend{SSHScriptOut: "__CLVCS[204]CLVCS__"}
	if vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID, state.GenerationID) {
		t.Fatal("unrelated process was accepted as broker owner")
	}
	if (realVCSBrokerRuntime{}).Inspect(backend, "stale", state).Host != vcsbroker.HostProbeDead {
		t.Fatal("authenticated endpoint did not override a mismatched owner PID")
	}
	if err := (realVCSBrokerRuntime{}).Stop(backend, "stale", state); err != nil {
		t.Fatal(err)
	}
	if !vcsBrokerProcessAlive(process.Process.Pid) {
		t.Fatal("stale broker state killed an unrelated reused PID")
	}
	if len(backend.SSHScriptCalls) != 1 || !strings.Contains(backend.SSHScriptCalls[0].Script, "stale-generation") {
		t.Fatalf("stale owner guest cleanup calls = %#v", backend.SSHScriptCalls)
	}
}

func TestVCSBrokerVMDeathRequiresThreeConsecutiveChecks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := vcsbroker.NewStateStore(home, "example", time.Second)
	statePath := store.StatePath
	state := vcsbroker.ServiceState{OwnerID: "vm-owner", GenerationID: "vm-generation", StatePath: statePath}
	if err := vcsbroker.WriteServiceState(statePath, state); err != nil {
		t.Fatal(err)
	}
	service := &runningVCSBrokerService{
		state:   state,
		backend: &vm.MockBackend{RunningProfiles: map[string]bool{"example": false}},
	}
	cfg := vcsBrokerServiceConfig{
		OwnerID: "vm-owner", GenerationID: "vm-generation", Profile: "example", StatePath: statePath,
		ConfigPath: home + "/config", ReadyPath: home + "/ready",
		RepairPath: home + "/repair", DrainPath: home + "/drain", DrainWait: time.Second,
	}
	signals := make(chan os.Signal)
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	maintenance := make(chan time.Time)
	go func() { done <- runVCSBrokerServiceLoop(service, cfg, signals, ticks, maintenance) }()
	for check := 1; check < vcsBrokerVMMisses; check++ {
		ticks <- time.Now()
		select {
		case err := <-done:
			t.Fatalf("service exited after %d failed VM checks: %v", check, err)
		case <-time.After(25 * time.Millisecond):
		}
	}
	ticks <- time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not exit after confirmed VM death")
	}
	remaining, err := vcsbroker.ReadServiceState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if remaining.OwnerID != "" {
		t.Fatalf("out-of-band VM death left service state %#v", remaining)
	}
}

func TestVCSBrokerVMRunningSampleResetsDeathConfirmation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := vcsbroker.NewStateStore(home, "transient", time.Second)
	state := vcsbroker.ServiceState{OwnerID: "transient-owner", GenerationID: "transient-generation", StatePath: store.StatePath}
	if err := vcsbroker.WriteServiceState(store.StatePath, state); err != nil {
		t.Fatal(err)
	}
	backend := &runningSequenceBackend{values: []bool{false, false, true, false, false, false}}
	service := &runningVCSBrokerService{state: state, backend: backend}
	cfg := vcsBrokerServiceConfig{
		OwnerID: state.OwnerID, GenerationID: state.GenerationID, Profile: "transient", StatePath: store.StatePath,
		ConfigPath: home + "/config", ReadyPath: home + "/ready", RepairPath: home + "/repair",
		DrainPath: home + "/drain", DrainWait: time.Second,
	}
	signals := make(chan os.Signal)
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- runVCSBrokerServiceLoop(service, cfg, signals, ticks, make(chan time.Time)) }()
	for check := 1; check <= 5; check++ {
		ticks <- time.Now()
		select {
		case err := <-done:
			t.Fatalf("service exited at sample %d after a running reset: %v", check, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	ticks <- time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not exit after three new consecutive stopped samples")
	}
}

func TestVCSBrokerVMDeathTakesProfileLockBeforeRemovingState(t *testing.T) {
	stateDir := t.TempDir()
	store := vcsbroker.NewStateStore(stateDir, "locked-death", time.Second)
	state := vcsbroker.ServiceState{OwnerID: "locked-owner", GenerationID: "locked-generation", StatePath: store.StatePath}
	if err := vcsbroker.WriteServiceState(store.StatePath, state); err != nil {
		t.Fatal(err)
	}
	locked, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	service := &runningVCSBrokerService{
		state: state, backend: &vm.MockBackend{RunningProfiles: map[string]bool{"locked-death": false}},
	}
	cfg := vcsBrokerServiceConfig{
		OwnerID: state.OwnerID, GenerationID: state.GenerationID, Profile: "locked-death", StatePath: state.StatePath,
		ConfigPath: filepath.Join(stateDir, "config"), ReadyPath: filepath.Join(stateDir, "ready"),
		RepairPath: filepath.Join(stateDir, "repair"), DrainPath: filepath.Join(stateDir, "drain"),
		LogPath: filepath.Join(stateDir, "log"), DrainWait: time.Second,
	}
	signals := make(chan os.Signal)
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- runVCSBrokerServiceLoop(service, cfg, signals, ticks, make(chan time.Time)) }()
	for range vcsBrokerVMMisses {
		ticks <- time.Now()
	}
	select {
	case err := <-done:
		t.Fatalf("VM-death cleanup bypassed the held profile lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("VM-death cleanup did not continue after profile lock release")
	}
}

func TestForcedVCSBrokerStopKillsCommandProcessGroup(t *testing.T) {
	childPath := filepath.Join(t.TempDir(), "child.pid")
	process := exec.Command("/bin/sh", "-c", `sleep 30 & child=$!; printf '%s\n' "$child" > "$CHILD_PATH"; wait`,
		"vcs-broker", "serve", "--owner", "hard-owner", "--generation", "hard-generation")
	process.Env = append(os.Environ(), "CHILD_PATH="+childPath)
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		_ = process.Wait()
	})
	var childPID int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(childPath)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 || !vcsBrokerProcessMatches(process.Process.Pid, "hard-owner", "hard-generation") {
		t.Fatalf("test broker process was not identifiable: daemon=%d child=%d", process.Process.Pid, childPID)
	}
	if err := stopVCSBrokerProcess(process.Process.Pid, "hard-owner", "hard-generation"); err != nil {
		t.Fatal(err)
	}
	_ = process.Wait()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		output, _ := exec.Command("ps", "-p", strconv.Itoa(childPID), "-o", "stat=").Output()
		state := strings.TrimSpace(string(output))
		if state == "" || strings.HasPrefix(state, "Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("VCS child PID %d survived forced daemon process-group stop", childPID)
}

func TestFailedReplacementCleanupLeavesSurvivingGenerationUntouched(t *testing.T) {
	stateDir := t.TempDir()
	store := vcsbroker.NewStateStore(stateDir, "example", time.Second)
	workspace := config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}
	old := newVCSBrokerServiceConfig(stateDir, store.StatePath, "shared-owner", "old-generation", "example", "colima", "/home/guest", workspace, nil, "old-hash", "old-build")
	process := exec.Command("/bin/sh", "-c", "sleep 30 & wait", "vcs-broker", "serve", "--owner", old.OwnerID, "--generation", old.GenerationID)
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		_ = process.Wait()
	})
	oldState := vcsbroker.ServiceState{
		OwnerID: old.OwnerID, GenerationID: old.GenerationID, BrokerPID: process.Process.Pid,
		StatePath: old.StatePath, ConfigPath: old.ConfigPath, ReadyPath: old.ReadyPath,
		RepairPath: old.RepairPath, DrainPath: old.DrainPath, ActivityPath: old.ActivityPath,
		TransitionPath: old.TransitionPath, RequestPath: old.RequestPath, SpoolDir: old.SpoolDir, LogPath: old.LogPath,
	}
	if err := vcsbroker.WriteServiceState(store.StatePath, oldState); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{old.ConfigPath, old.ActivityPath, old.LogPath} {
		if err := os.WriteFile(path, []byte("old generation\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(old.SpoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	spoolMarker := filepath.Join(old.SpoolDir, "open-generation-marker")
	if err := os.WriteFile(spoolMarker, []byte("old output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := replacementVCSBrokerServiceConfig(stateDir, old, "failed-generation")
	cleanupFailedVCSBrokerStart(failed, 0)
	if !vcsBrokerProcessAlive(process.Process.Pid) {
		t.Fatal("failed replacement cleanup killed the surviving generation")
	}
	current, err := vcsbroker.ReadServiceState(store.StatePath)
	if err != nil || current.GenerationID != old.GenerationID {
		t.Fatalf("failed replacement changed current generation: %#v error=%v", current, err)
	}
	for _, path := range []string{old.ConfigPath, old.ActivityPath, old.LogPath, spoolMarker} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed replacement removed surviving generation path %q: %v", path, err)
		}
	}
}

func TestConcurrentBrokerGenerationsAreTargetedByExactIdentity(t *testing.T) {
	start := func(generation string) *exec.Cmd {
		process := exec.Command("/bin/sh", "-c", "sleep 30 & wait", "vcs-broker", "serve", "--owner", "shared-owner", "--generation", generation)
		process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := process.Start(); err != nil {
			t.Fatal(err)
		}
		return process
	}
	first := start("first-generation")
	second := start("second-generation")
	t.Cleanup(func() {
		_ = syscall.Kill(-first.Process.Pid, syscall.SIGKILL)
		_ = syscall.Kill(-second.Process.Pid, syscall.SIGKILL)
		_ = first.Wait()
		_ = second.Wait()
	})
	deadline := time.Now().Add(time.Second)
	for !vcsBrokerProcessMatches(first.Process.Pid, "shared-owner", "first-generation") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !vcsBrokerProcessMatches(first.Process.Pid, "shared-owner", "first-generation") ||
		vcsBrokerProcessMatches(first.Process.Pid, "shared-owner", "second-generation") {
		t.Fatal("first generation process identity was not exact")
	}
	if err := stopVCSBrokerProcess(first.Process.Pid, "shared-owner", "first-generation"); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	if !vcsBrokerProcessAlive(second.Process.Pid) || !vcsBrokerProcessMatches(second.Process.Pid, "shared-owner", "second-generation") {
		t.Fatal("stopping first generation affected the second generation")
	}
}

func TestSIGKILLedBrokerWatchdogKillsRunningVCSChild(t *testing.T) {
	if os.Getenv("CLOISTER_VCS_HELPER") != "" {
		return
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "watchdog")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nCLOISTER_VCS_HELPER=watchdog exec %q -test.run '^TestVCSBrokerSubprocessHelper$' -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(dir, "child.pid")
	completedPath := filepath.Join(dir, "completed")
	daemon := exec.Command(os.Args[0], "-test.run", "^TestVCSBrokerSubprocessHelper$")
	daemon.Env = append(os.Environ(),
		"CLOISTER_VCS_HELPER=daemon",
		"CLOISTER_VCS_WATCHDOG="+wrapper,
		"CLOISTER_VCS_CHILD_PID="+childPath,
		"CLOISTER_VCS_CHILD_COMPLETED="+completedPath,
	)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL)
		_ = daemon.Wait()
	})
	childPID := waitForPIDFile(t, childPath)
	newGeneration := exec.Command("sleep", "30")
	newGeneration.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := newGeneration.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-newGeneration.Process.Pid, syscall.SIGKILL); _ = newGeneration.Wait() })
	if err := syscall.Kill(daemon.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = daemon.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, _ := exec.Command("ps", "-p", strconv.Itoa(childPID), "-o", "stat=").Output()
		state := strings.TrimSpace(string(output))
		if state == "" || strings.HasPrefix(state, "Z") {
			if !vcsBrokerProcessAlive(newGeneration.Process.Pid) {
				t.Fatal("old watchdog affected the new daemon generation")
			}
			if _, err := os.Stat(completedPath); !os.IsNotExist(err) {
				t.Fatalf("orphaned VCS child completed after broker SIGKILL: %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("VCS child PID %d survived a raw broker SIGKILL", childPID)
}

func TestDetachedBrokerDrainsRealGitThroughPostBarrierBeforeExit(t *testing.T) {
	if os.Getenv("CLOISTER_VCS_HELPER") != "" {
		return
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init", "-q")
	runGit("config", "user.name", "Broker Test")
	runGit("config", "user.email", "broker@example.invalid")
	runGit("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "change.txt")
	portPath := filepath.Join(dir, "port")
	postPath := filepath.Join(dir, "post")
	releasePath := filepath.Join(dir, "release")
	refreshPath := filepath.Join(dir, "refresh")
	signalReadyPath := filepath.Join(dir, "signal-ready")
	drainReadyPath := filepath.Join(dir, "drain-ready")
	daemon := exec.Command(os.Args[0], "-test.run", "^TestVCSBrokerSubprocessHelper$")
	daemon.Env = append(os.Environ(),
		"CLOISTER_VCS_HELPER=drain-daemon",
		"CLOISTER_VCS_REPO="+repo,
		"CLOISTER_VCS_PORT="+portPath,
		"CLOISTER_VCS_POST="+postPath,
		"CLOISTER_VCS_RELEASE="+releasePath,
		"CLOISTER_VCS_REFRESH="+refreshPath,
		"CLOISTER_VCS_SIGNAL_READY="+signalReadyPath,
		"CLOISTER_VCS_DRAIN_READY="+drainReadyPath,
	)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL)
		_ = daemon.Wait()
	})
	port := waitForPIDFile(t, portPath)
	waitForFile(t, signalReadyPath)

	form := url.Values{
		"tool": {"git"}, "cwd": {"/home/guest/workspaces/project"},
		"arg": {"commit", "-m", "detached drain"},
	}
	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/exec", port), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer detached-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	type responseResult struct {
		status  int
		trailer string
		err     error
	}
	result := make(chan responseResult, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			result <- responseResult{err: requestErr}
			return
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		result <- responseResult{status: response.StatusCode, trailer: response.Trailer.Get("X-Cloister-Exit-Code"), err: readErr}
	}()
	waitForFile(t, postPath)
	if err := syscall.Kill(daemon.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, drainReadyPath)
	if status := vcsbroker.ProbeHost(port, "detached-token"); status != vcsbroker.HostProbeDraining {
		t.Fatalf("detached daemon health during drain = %v", status)
	}
	if err := os.WriteFile(releasePath, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.status != http.StatusOK || got.trailer != "0" {
			t.Fatalf("in-flight response = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight response did not complete")
	}
	if err := daemon.Wait(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, refreshPath)
	if subject := runGit("log", "-1", "--format=%s"); subject != "detached drain" {
		t.Fatalf("host commit subject = %q", subject)
	}
}

func TestSIGKILLLeavesNoNamedBrokerResponseSpool(t *testing.T) {
	if os.Getenv("CLOISTER_VCS_HELPER") != "" {
		return
	}
	dir := t.TempDir()
	spoolDir := filepath.Join(dir, "spools")
	if err := os.Mkdir(spoolDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spoolDir, "stale"), []byte("old secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	portPath := filepath.Join(dir, "port")
	readyPath := filepath.Join(dir, "spooled")
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command(os.Args[0], "-test.run", "^TestVCSBrokerSubprocessHelper$")
	daemon.Env = append(os.Environ(), "CLOISTER_VCS_HELPER=spool-daemon", "CLOISTER_VCS_PORT="+portPath, "CLOISTER_VCS_SPOOL_DIR="+spoolDir, "CLOISTER_VCS_SIGNAL_READY="+readyPath, "CLOISTER_VCS_REPO="+repo)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL); _ = daemon.Wait() })
	port := waitForPIDFile(t, portPath)
	form := url.Values{"tool": {"git"}, "cwd": {"/home/guest/workspaces/project"}, "arg": {"status"}}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/exec", port), strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer spool-token")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	go func() { _, _ = http.DefaultClient.Do(req) }()
	waitForFile(t, readyPath)
	entries, err := os.ReadDir(spoolDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("named response spools before crash=%v error=%v", entries, err)
	}
	info, err := os.Stat(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("spool directory mode=%v", info.Mode().Perm())
	}
	if err := syscall.Kill(daemon.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = daemon.Wait()
	entries, err = os.ReadDir(spoolDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("named response spools after crash=%v error=%v", entries, err)
	}
}

func TestDetachedBrokerTokenIsAbsentFromProcessAndLogs(t *testing.T) {
	if os.Getenv("CLOISTER_VCS_HELPER") != "" {
		return
	}
	dir := t.TempDir()
	portPath := filepath.Join(dir, "port")
	logPath := filepath.Join(dir, "service.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command(os.Args[0], "-test.run", "^TestVCSBrokerSubprocessHelper$")
	process.Env = append(os.Environ(), "CLOISTER_VCS_HELPER=token-daemon", "CLOISTER_VCS_PORT="+portPath)
	process.Stdout = logFile
	process.Stderr = logFile
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
		_ = logFile.Close()
	})
	port := waitForPIDFile(t, portPath)
	if status := vcsbroker.ProbeHost(port, "detached-private-token"); status != vcsbroker.HostProbeHealthy {
		t.Fatalf("detached privacy helper status = %v", status)
	}
	processView, err := exec.Command("ps", "eww", "-p", strconv.Itoa(process.Process.Pid), "-o", "command=").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(processView), "detached-private-token") {
		t.Fatalf("broker token exposed in process argv or environment: %s", processView)
	}
	if err := process.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	logged, err := io.ReadAll(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "detached-private-token") {
		t.Fatalf("broker token exposed in log: %s", logged)
	}
}

func TestDetachedBrokerConfigAndBuildTransitionsDrainRealGit(t *testing.T) {
	if os.Getenv("CLOISTER_VCS_HELPER") != "" {
		return
	}
	for _, kind := range []string{"config", "build"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			if err := os.Mkdir(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Broker Test"}, {"config", "user.email", "broker@example.invalid"}, {"config", "commit.gpgsign", "false"}} {
				if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
			}
			if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("change\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("git", "-C", repo, "add", "change.txt").CombinedOutput(); err != nil {
				t.Fatalf("git add: %v: %s", err, output)
			}
			paths := map[string]string{}
			for _, name := range []string{"port", "ready", "post", "release", "refresh", "gate", "applied"} {
				paths[name] = filepath.Join(dir, name)
			}
			daemon := exec.Command(os.Args[0], "-test.run", "^TestVCSBrokerSubprocessHelper$")
			daemon.Env = append(os.Environ(),
				"CLOISTER_VCS_HELPER=transition-daemon", "CLOISTER_VCS_KIND="+kind,
				"CLOISTER_VCS_REPO="+repo, "CLOISTER_VCS_STATE_DIR="+dir,
				"CLOISTER_VCS_PORT="+paths["port"], "CLOISTER_VCS_SIGNAL_READY="+paths["ready"],
				"CLOISTER_VCS_POST="+paths["post"], "CLOISTER_VCS_RELEASE="+paths["release"],
				"CLOISTER_VCS_REFRESH="+paths["refresh"], "CLOISTER_VCS_DRAIN_READY="+paths["gate"],
				"CLOISTER_VCS_APPLIED="+paths["applied"],
			)
			daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := daemon.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL)
				_ = daemon.Wait()
			})
			port := waitForPIDFile(t, paths["port"])
			waitForFile(t, paths["ready"])
			form := url.Values{"tool": {"git"}, "cwd": {"/home/guest/workspaces/project"}, "arg": {"commit", "-m", kind + " transition"}}
			req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/exec", port), strings.NewReader(form.Encode()))
			req.Header.Set("Authorization", "Bearer transition-token")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			result := make(chan string, 1)
			go func() {
				response, requestErr := http.DefaultClient.Do(req)
				if requestErr != nil {
					result <- requestErr.Error()
					return
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				result <- fmt.Sprintf("%d:%s:%v", response.StatusCode, response.Trailer.Get("X-Cloister-Exit-Code"), readErr)
			}()
			waitForFile(t, paths["post"])
			if err := daemon.Process.Signal(syscall.SIGUSR2); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, paths["gate"])
			if status := vcsbroker.ProbeHost(port, "transition-token"); status != vcsbroker.HostProbeDraining {
				t.Fatalf("transition gate status = %v", status)
			}
			if err := os.WriteFile(paths["release"], []byte("release\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := <-result; got != "200:0:<nil>" {
				t.Fatalf("transition command response = %q", got)
			}
			waitForFile(t, paths["refresh"])
			waitForFile(t, paths["applied"])
			if kind == "config" {
				if err := daemon.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			if err := daemon.Wait(); err != nil {
				t.Fatal(err)
			}
			if kind == "build" {
				state, err := vcsbroker.ReadServiceState(vcsbroker.NewStateStore(dir, "example", time.Second).StatePath)
				if err != nil || state.BrokerPID == daemon.Process.Pid || vcsbroker.ProbeHost(state.HostPort, state.Token) != vcsbroker.HostProbeHealthy {
					t.Fatalf("real replacement state=%#v error=%v", state, err)
				}
				_ = syscall.Kill(-state.BrokerPID, syscall.SIGKILL)
			}
		})
	}
}

func TestEnsureUnhealthyDetachedBrokerDrainsRealGitBeforeReplacement(t *testing.T) {
	if os.Getenv("CLOISTER_VCS_HELPER") != "" {
		return
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Broker Test"}, {"config", "user.email", "broker@example.invalid"}, {"config", "commit.gpgsign", "false"}} {
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repo, "add", "change.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	paths := map[string]string{}
	for _, name := range []string{"port", "ready", "post", "release", "refresh", "gate", "applied"} {
		paths[name] = filepath.Join(dir, name)
	}
	daemon := exec.Command(os.Args[0], "-test.run", "^TestVCSBrokerSubprocessHelper$")
	daemon.Env = append(os.Environ(),
		"CLOISTER_VCS_HELPER=transition-daemon", "CLOISTER_VCS_KIND=build",
		"CLOISTER_VCS_REPO="+repo, "CLOISTER_VCS_STATE_DIR="+dir,
		"CLOISTER_VCS_PORT="+paths["port"], "CLOISTER_VCS_SIGNAL_READY="+paths["ready"],
		"CLOISTER_VCS_POST="+paths["post"], "CLOISTER_VCS_RELEASE="+paths["release"],
		"CLOISTER_VCS_REFRESH="+paths["refresh"], "CLOISTER_VCS_DRAIN_READY="+paths["gate"],
		"CLOISTER_VCS_APPLIED="+paths["applied"],
	)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL); _ = daemon.Wait() })
	port := waitForPIDFile(t, paths["port"])
	waitForFile(t, paths["ready"])
	form := url.Values{"tool": {"git"}, "cwd": {"/home/guest/workspaces/project"}, "arg": {"commit", "-m", "unhealthy transition"}}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/exec", port), strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer transition-token")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	result := make(chan string, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			result <- requestErr.Error()
			return
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		result <- fmt.Sprintf("%d:%s:%v", response.StatusCode, response.Trailer.Get("X-Cloister-Exit-Code"), readErr)
	}()
	waitForFile(t, paths["post"])
	deadListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := deadListener.Addr().(*net.TCPAddr).Port
	_ = deadListener.Close()
	store := vcsbroker.NewStateStore(dir, "example", time.Second)
	state, err := vcsbroker.ReadServiceState(store.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	state.HostPort = deadPort
	if err := vcsbroker.WriteServiceState(store.StatePath, state); err != nil {
		t.Fatal(err)
	}
	runtime := &unhealthyDetachedRuntime{pid: daemon.Process.Pid}
	manager := &vcsBrokerManager{stateDir: dir, lockWait: time.Second, runtime: runtime, buildID: "old-build", newID: newVCSBrokerID}
	profile := &config.Profile{Backend: "colima", StartDir: dir, Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}}
	started := time.Now()
	err = manager.ensure(vcsTestBackend(), "example", "colima", profile)
	if err == nil || !strings.Contains(err.Error(), "graceful replacement requested") || !strings.Contains(err.Error(), "git commit") {
		t.Fatalf("unhealthy ensure warning = %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("unhealthy ensure blocked on the daemon drain")
	}
	waitForFile(t, paths["gate"])
	if runtime.forceStop.Load() {
		t.Fatal("unhealthy live daemon was force-stopped")
	}
	if err := os.WriteFile(paths["release"], []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := <-result; got != "200:0:<nil>" {
		t.Fatalf("in-flight command response = %q", got)
	}
	waitForFile(t, paths["refresh"])
	if subject, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%s").CombinedOutput(); err != nil || strings.TrimSpace(string(subject)) != "unhealthy transition" {
		t.Fatalf("host commit=%q error=%v", subject, err)
	}
	if err := daemon.Wait(); err != nil {
		t.Fatal(err)
	}
	replacement, err := vcsbroker.ReadServiceState(vcsbroker.NewStateStore(dir, "example", time.Second).StatePath)
	if err == nil && replacement.BrokerPID != daemon.Process.Pid {
		_ = syscall.Kill(-replacement.BrokerPID, syscall.SIGKILL)
	}
}

func TestVCSBrokerSubprocessHelper(t *testing.T) {
	switch os.Getenv("CLOISTER_VCS_HELPER") {
	case "daemon":
		runner := vcsbroker.NewSupervisedRunner(os.Getenv("CLOISTER_VCS_WATCHDOG"), syscall.Getpgrp())
		script := `printf '%s\n' "$$" > "$CLOISTER_VCS_CHILD_PID"; sleep 30; : > "$CLOISTER_VCS_CHILD_COMPLETED"`
		_, _ = runner.Run(context.Background(), "/bin/sh", []string{"-c", script}, "", os.Environ(), io.Discard)
		os.Exit(0)
	case "watchdog":
		separator := -1
		for i, arg := range os.Args {
			if arg == "--" {
				separator = i
				break
			}
		}
		if separator < 0 || len(os.Args) != separator+5 || os.Args[separator+1] != "vcs-broker" || os.Args[separator+2] != "watch-child" {
			os.Exit(2)
		}
		processGroup, _ := strconv.Atoi(os.Args[separator+3])
		childPID, _ := strconv.Atoi(os.Args[separator+4])
		if err := runVCSBrokerChildWatchdog(processGroup, childPID); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	case "drain-daemon":
		repo, err := filepath.EvalSymlinks(os.Getenv("CLOISTER_VCS_REPO"))
		if err != nil {
			os.Exit(4)
		}
		mapper, err := vcsbroker.NewMapper("/home/guest", []broker.SessionSpec{{
			Profile: "example", ProjectID: "project", Name: "project",
			HostRoot: repo, GuestRoot: "~/workspaces/project",
		}})
		if err != nil {
			os.Exit(5)
		}
		barrier := &subprocessDrainBarrier{
			post: os.Getenv("CLOISTER_VCS_POST"), release: os.Getenv("CLOISTER_VCS_RELEASE"),
			refresh: os.Getenv("CLOISTER_VCS_REFRESH"),
		}
		server, err := vcsbroker.StartServer(vcsbroker.NewProxy(barrier, mapper, nil), "detached-token")
		if err != nil {
			os.Exit(6)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv("CLOISTER_VCS_SIGNAL_READY"), []byte("ready\n"), 0o600); err != nil {
			os.Exit(7)
		}
		if err := os.WriteFile(os.Getenv("CLOISTER_VCS_PORT"), []byte(strconv.Itoa(server.Port())+"\n"), 0o600); err != nil {
			os.Exit(7)
		}
		<-signals
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		drained := make(chan error, 1)
		go func() {
			_, drainErr := server.Drain(ctx)
			drained <- drainErr
		}()
		for !server.IsDraining() {
			time.Sleep(time.Millisecond)
		}
		if err := os.WriteFile(os.Getenv("CLOISTER_VCS_DRAIN_READY"), []byte("draining\n"), 0o600); err != nil {
			os.Exit(7)
		}
		err = <-drained
		cancel()
		if err != nil {
			os.Exit(8)
		}
		os.Exit(0)
	case "token-daemon":
		server, err := vcsbroker.StartServer(nil, "detached-private-token")
		if err != nil {
			os.Exit(9)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv("CLOISTER_VCS_PORT"), []byte(strconv.Itoa(server.Port())+"\n"), 0o600); err != nil {
			os.Exit(10)
		}
		<-signals
		_ = server.Close()
		os.Exit(0)
	case "spool-daemon":
		repo, err := filepath.EvalSymlinks(os.Getenv("CLOISTER_VCS_REPO"))
		if err != nil {
			os.Exit(33)
		}
		mapper, err := vcsbroker.NewMapper("/home/guest", []broker.SessionSpec{{Profile: "example", ProjectID: "project", Name: "project", HostRoot: repo, GuestRoot: "~/workspaces/project"}})
		if err != nil {
			os.Exit(34)
		}
		server, err := vcsbroker.StartServerWithSpoolDir(vcsbroker.NewProxy(&broker.Mock{}, mapper, subprocessSpoolRunner{ready: os.Getenv("CLOISTER_VCS_SIGNAL_READY")}), "spool-token", os.Getenv("CLOISTER_VCS_SPOOL_DIR"))
		if err != nil {
			os.Exit(35)
		}
		if err := os.WriteFile(os.Getenv("CLOISTER_VCS_PORT"), []byte(strconv.Itoa(server.Port())+"\n"), 0o600); err != nil {
			os.Exit(36)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		<-signals
		_ = server.Close()
		os.Exit(0)
	case "managed-probe-daemon":
		generation, _ := strconv.Atoi(os.Getenv("CLOISTER_VCS_GENERATION"))
		server, err := vcsbroker.StartServer(nil, fmt.Sprintf("managed-probe-token-%d", generation))
		if err != nil {
			os.Exit(11)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv("CLOISTER_VCS_PORT"), []byte(strconv.Itoa(server.Port())+"\n"), 0o600); err != nil {
			os.Exit(12)
		}
		<-signals
		_ = server.Close()
		os.Exit(0)
	case "transition-daemon":
		runVCSBrokerTransitionHelper()
		os.Exit(0)
	case "transition-replacement-daemon":
		server, err := vcsbroker.StartServer(nil, "transition-replacement-token")
		if err != nil {
			os.Exit(30)
		}
		if err := os.WriteFile(os.Getenv("CLOISTER_VCS_PORT"), []byte(strconv.Itoa(server.Port())+"\n"), 0o600); err != nil {
			os.Exit(31)
		}
		if err := os.WriteFile(os.Getenv("CLOISTER_VCS_APPLIED"), []byte("guest refreshed\n"), 0o600); err != nil {
			os.Exit(32)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		<-signals
		_ = server.Close()
		os.Exit(0)
	}
}

func runVCSBrokerTransitionHelper() {
	repo, err := filepath.EvalSymlinks(os.Getenv("CLOISTER_VCS_REPO"))
	if err != nil {
		os.Exit(20)
	}
	spec := broker.SessionSpec{Profile: "example", ProjectID: "project", Name: "project", HostRoot: repo, GuestRoot: "~/workspaces/project"}
	mapper, err := vcsbroker.NewMapper("/home/guest", []broker.SessionSpec{spec})
	if err != nil {
		os.Exit(21)
	}
	barrier := &subprocessDrainBarrier{post: os.Getenv("CLOISTER_VCS_POST"), release: os.Getenv("CLOISTER_VCS_RELEASE"), refresh: os.Getenv("CLOISTER_VCS_REFRESH")}
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(barrier, mapper, nil), "transition-token")
	if err != nil {
		os.Exit(22)
	}
	stateDir := os.Getenv("CLOISTER_VCS_STATE_DIR")
	store := vcsbroker.NewStateStore(stateDir, "example", time.Second)
	workspace := config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}
	hash, _ := vcsBrokerConfigHash("/home/guest", workspace, []broker.SessionSpec{spec})
	state := vcsbroker.ServiceState{OwnerID: "transition-owner", GenerationID: "transition-old-generation", BrokerPID: os.Getpid(), TunnelPID: 200, HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: "transition-token", ConfigHash: hash, BuildID: "old-build", TunnelTarget: "vm.test", StatePath: store.StatePath, ConfigPath: filepath.Join(stateDir, "service.json"), ReadyPath: filepath.Join(stateDir, "ready.json"), RepairPath: filepath.Join(stateDir, "repair.json"), DrainPath: filepath.Join(stateDir, "drain.json"), ActivityPath: filepath.Join(stateDir, "activity.json"), TransitionPath: filepath.Join(stateDir, "transition.json"), RequestPath: filepath.Join(stateDir, "request.json"), SpoolDir: filepath.Join(stateDir, "spools"), LogPath: filepath.Join(stateDir, "service.log")}
	_ = vcsbroker.WriteServiceState(state.StatePath, state)
	current := vcsBrokerServiceConfig{OwnerID: state.OwnerID, GenerationID: state.GenerationID, Profile: "example", Backend: "colima", GuestHome: "/home/guest", Specs: []broker.SessionSpec{spec}, Workspace: workspace, ConfigHash: hash, BuildID: state.BuildID, StatePath: state.StatePath, ConfigPath: state.ConfigPath, ReadyPath: state.ReadyPath, RepairPath: state.RepairPath, DrainPath: state.DrainPath, ActivityPath: state.ActivityPath, TransitionPath: state.TransitionPath, RequestPath: state.RequestPath, SpoolDir: state.SpoolDir, LogPath: state.LogPath, DrainWait: 30 * time.Second}
	desired := current
	if os.Getenv("CLOISTER_VCS_KIND") == "config" {
		desired.Workspace.Ignore = []string{"generated/"}
		desired.ConfigHash, _ = vcsBrokerConfigHash(desired.GuestHome, desired.Workspace, desired.Specs)
	} else {
		desired.BuildID = "new-build"
		desired = replacementVCSBrokerServiceConfig(stateDir, desired, "transition-new-generation")
	}
	_ = writePrivateJSON(current.RequestPath, desired)
	service := &runningVCSBrokerService{state: state, backend: &vm.MockBackend{}, server: server, tunnel: tunnel.ReverseForwardOwner{OwnerID: state.GenerationID, PID: state.TunnelPID, HostPort: state.HostPort, GuestPort: state.GuestPort, Target: state.TunnelTarget}}
	server.SetStatusObserver(service.publishActivity)
	newWorkspaceBroker = func() (broker.SyncBroker, error) { return barrier, nil }
	newVCSBrokerHostRunnerFn = func() (vcsbroker.HostCommandRunner, error) { return nil, nil }
	deployVCSBrokerGuestFn = func(vm.Backend, string, int, string, string) error {
		return os.WriteFile(os.Getenv("CLOISTER_VCS_APPLIED"), []byte("applied\n"), 0o600)
	}
	stopVCSBrokerTunnelFn = func(string, string, tunnel.ReverseForwardOwner) bool { return true }
	startVCSBrokerReplacementFn = func(cfg vcsBrokerServiceConfig) (vcsbroker.ServiceState, error) {
		portPath := os.Getenv("CLOISTER_VCS_APPLIED") + ".replacement-port"
		replacementProcess := exec.Command(os.Args[0], "-test.run", "^TestVCSBrokerSubprocessHelper$")
		replacementProcess.Env = append(os.Environ(), "CLOISTER_VCS_HELPER=transition-replacement-daemon", "CLOISTER_VCS_PORT="+portPath, "CLOISTER_VCS_APPLIED="+os.Getenv("CLOISTER_VCS_APPLIED"))
		replacementProcess.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := replacementProcess.Start(); err != nil {
			return vcsbroker.ServiceState{}, err
		}
		port := 0
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(portPath)
			if err == nil {
				port, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				if port > 0 {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if port == 0 {
			_ = syscall.Kill(-replacementProcess.Process.Pid, syscall.SIGKILL)
			return vcsbroker.ServiceState{}, errors.New("replacement daemon did not become ready")
		}
		replacement := state
		replacement.BrokerPID = replacementProcess.Process.Pid
		replacement.TunnelPID++
		replacement.HostPort = port
		replacement.Token = "transition-replacement-token"
		replacement.BuildID = cfg.BuildID
		replacement.GenerationID = cfg.GenerationID
		replacement.ConfigPath = cfg.ConfigPath
		replacement.ReadyPath = cfg.ReadyPath
		replacement.RepairPath = cfg.RepairPath
		replacement.DrainPath = cfg.DrainPath
		replacement.ActivityPath = cfg.ActivityPath
		replacement.TransitionPath = cfg.TransitionPath
		replacement.RequestPath = cfg.RequestPath
		replacement.SpoolDir = cfg.SpoolDir
		replacement.LogPath = cfg.LogPath
		if err := vcsbroker.WriteServiceState(cfg.StatePath, replacement); err != nil {
			return vcsbroker.ServiceState{}, err
		}
		return replacement, nil
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGUSR2)
	go func() {
		for !server.IsDraining() {
			time.Sleep(time.Millisecond)
		}
		_ = os.WriteFile(os.Getenv("CLOISTER_VCS_DRAIN_READY"), []byte("draining\n"), 0o600)
	}()
	if err := os.WriteFile(os.Getenv("CLOISTER_VCS_SIGNAL_READY"), []byte("ready\n"), 0o600); err != nil {
		os.Exit(23)
	}
	if err := os.WriteFile(os.Getenv("CLOISTER_VCS_PORT"), []byte(strconv.Itoa(server.Port())+"\n"), 0o600); err != nil {
		os.Exit(24)
	}
	if err := runVCSBrokerServiceLoop(service, current, signals, make(chan time.Time), make(chan time.Time)); err != nil {
		os.Exit(25)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for child PID at %s", path)
	return 0
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestStopVMInvokesVCSLifecycleBeforeBackendStop(t *testing.T) {
	previous := stopVCSBrokerFn
	defer func() { stopVCSBrokerFn = previous }()
	called := false
	stopVCSBrokerFn = func(backend vm.Backend, profile string) error {
		called = true
		if len(backend.(*vm.MockBackend).StopCalls) != 0 {
			t.Fatal("VM stopped before its VCS broker")
		}
		return nil
	}
	backend := &vm.MockBackend{}
	profile := &config.Profile{Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeVirtiofs}}
	if err := stopVM(backend, "lifecycle", profile, false, false); err != nil {
		t.Fatal(err)
	}
	if !called || len(backend.StopCalls) != 1 {
		t.Fatalf("VCS stop called=%v backend stops=%v", called, backend.StopCalls)
	}
}

func TestStopBeforeResetStopsBrokerWhenVMIsAlreadyDown(t *testing.T) {
	previous := stopVCSBrokerFn
	defer func() { stopVCSBrokerFn = previous }()
	called := false
	stopVCSBrokerFn = func(_ vm.Backend, profile string) error {
		called = profile == "reset-target"
		return nil
	}
	backend := &vm.MockBackend{RunningProfiles: map[string]bool{"reset-target": false}}
	if err := stopBeforeReset(backend, "reset-target", &config.Profile{}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("destructive reset could proceed with a stale VCS broker")
	}
}

func TestEnsureVCSBrokerFailureWarnsWithoutBlockingVMAccess(t *testing.T) {
	previous := ensureVCSBrokerFn
	defer func() { ensureVCSBrokerFn = previous }()
	ensureVCSBrokerFn = func(vm.Backend, string, *config.Profile) error {
		return fmt.Errorf("timed out waiting for profile lock")
	}
	returned := false
	stderr := captureStderr(t, func() {
		ensureVCSBrokerWithWarning(&vm.MockBackend{}, "example", &config.Profile{})
		returned = true
	})
	if !returned || !strings.Contains(stderr, "VM access will continue") || !strings.Contains(stderr, "cloister repair example") {
		t.Fatalf("nonfatal ensure warning = %q", stderr)
	}
}

func TestEntryBrokerEnsureUsesDetachedLauncher(t *testing.T) {
	previous := launchVCSBrokerEnsureFn
	called := make(chan string, 1)
	launchVCSBrokerEnsureFn = func(profile string) error {
		called <- profile
		return nil
	}
	t.Cleanup(func() { launchVCSBrokerEnsureFn = previous })
	ensureVCSBrokerAsyncWithWarning("example")
	select {
	case profile := <-called:
		if profile != "example" {
			t.Fatalf("detached ensure profile = %q", profile)
		}
	case <-time.After(time.Second):
		t.Fatal("entry did not launch detached broker ensure")
	}
}

func TestEntryBrokerEnsureDoesNotWaitForDetachedRepair(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "ensure-helper")
	marker := filepath.Join(t.TempDir(), "finished")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 1\nprintf 'done\\n' > \"$ENSURE_FINISHED\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENSURE_FINISHED", marker)
	previousExecutable := vcsBrokerExecutableFn
	previousLauncher := launchVCSBrokerEnsureFn
	vcsBrokerExecutableFn = func() (string, error) { return helper, nil }
	launchVCSBrokerEnsureFn = launchVCSBrokerEnsure
	t.Cleanup(func() {
		vcsBrokerExecutableFn = previousExecutable
		launchVCSBrokerEnsureFn = previousLauncher
	})
	started := time.Now()
	ensureVCSBrokerAsyncWithWarning("example")
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("entry waited %s for detached broker repair", elapsed)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("detached ensure helper did not finish after entry continued")
}
