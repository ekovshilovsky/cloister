package cmd

import (
	"context"
	"fmt"
	"io"
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
	return &fakePersistentVCSRuntime{brokerAlive: true, tunnelAlive: true, endpointHealthy: true}
}

func (f *fakePersistentVCSRuntime) Inspect(_ vm.Backend, _ string, state vcsbroker.ServiceState) vcsBrokerHealth {
	f.mu.Lock()
	defer f.mu.Unlock()
	host := vcsbroker.HostProbeDead
	if f.brokerAlive {
		host = vcsbroker.HostProbeHealthy
	}
	if f.brokerAlive && state.Phase == "stopping" {
		host = vcsbroker.HostProbeDraining
	}
	return vcsBrokerHealth{Host: host, Tunnel: f.brokerAlive && f.tunnelAlive && f.endpointHealthy}
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
	return nil
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
	f.tunnelAlive = true
	f.endpointHealthy = true
	state := vcsbroker.ServiceState{
		OwnerID: cfg.OwnerID, BrokerPID: 1000 + f.starts,
		TunnelPID: 2000 + f.starts, HostPort: 30000 + f.starts,
		GuestPort: vcsBrokerGuestPort, Token: fmt.Sprintf("token-%d", f.starts),
		ConfigHash: cfg.ConfigHash, BuildID: cfg.BuildID, TunnelTarget: "vm.test",
		StatePath: cfg.StatePath, ConfigPath: cfg.ConfigPath, ReadyPath: cfg.ReadyPath,
		RepairPath: cfg.RepairPath, DrainPath: cfg.DrainPath, LogPath: cfg.LogPath,
	}
	// The standalone service, not the ensure caller, publishes its token and
	// process identity.
	if err := vcsbroker.WriteServiceState(cfg.StatePath, state); err != nil {
		return vcsbroker.ServiceState{}, err
	}
	return vcsbroker.ReadServiceState(cfg.StatePath)
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
			OwnerID: "repair-owner", BrokerPID: 111, TunnelPID: 221, HostPort: server.Port(),
			GuestPort: vcsBrokerGuestPort, Token: "repair-token", TunnelTarget: "vm.test",
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		backend: &vm.MockBackend{}, server: server,
		tunnel: tunnel.ReverseForwardOwner{OwnerID: "repair-owner", PID: 221, HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Target: "vm.test"},
	}
	cfg := vcsBrokerServiceConfig{
		OwnerID: "repair-owner", Profile: "example", StatePath: service.state.StatePath,
		RepairPath: filepath.Join(filepath.Dir(service.state.StatePath), "repair.json"), DrainWait: time.Second,
	}
	signals := make(chan os.Signal)
	ticks := make(chan time.Time)
	maintenance := make(chan time.Time)
	loopDone := make(chan error, 1)
	go func() { loopDone <- runVCSBrokerServiceLoop(service, cfg, signals, ticks, maintenance) }()
	signals <- syscall.SIGUSR1
	maintenance <- time.Now()
	select {
	case <-repaired:
		t.Fatal("tunnel repair ran before the in-flight command completed")
	case <-time.After(25 * time.Millisecond):
	}
	if server.IsDraining() || service.state.BrokerPID != 111 || service.state.TunnelPID != 221 {
		t.Fatalf("pending repair paused or replaced an active daemon: draining=%v state=%#v", server.IsDraining(), service.state)
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
	maintenance <- time.Now()
	select {
	case <-repaired:
	case <-time.After(time.Second):
		t.Fatal("idle tunnel repair did not redeploy guest configuration")
	}
	maintenance <- time.Now()
	if server.IsDraining() || service.state.BrokerPID != 111 || service.state.TunnelPID != 222 {
		t.Fatalf("idle tunnel repair state: draining=%v state=%#v", server.IsDraining(), service.state)
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
	if err := manager.ensure(vcsTestBackend(), "changed", "colima", profile); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("deferred config restart blocked ensure for %s", elapsed)
	}
	after := readVCSServiceState(t, manager, "changed")
	if starts, stops := runtime.counts(); starts != 1 || stops != 0 || runtime.restartCount() != 1 {
		t.Fatalf("config restart starts=%d stops=%d requests=%d, want 1, 0, 1", starts, stops, runtime.restartCount())
	}
	if before != after {
		t.Fatalf("ensure changed a service before its idle reload: before=%#v after=%#v", before, after)
	}
}

func TestVCSBrokerDeferredConfigReloadWaitsForIdleCommand(t *testing.T) {
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
		OwnerID: "reload-owner", BrokerPID: os.Getpid(), TunnelPID: 200,
		HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: "reload-token",
		ConfigHash: currentHash, BuildID: "same-build", TunnelTarget: "vm.test",
		StatePath: store.StatePath, ConfigPath: filepath.Join(stateDir, "service.json"),
		ReadyPath: filepath.Join(stateDir, "ready.json"), RepairPath: filepath.Join(stateDir, "repair.json"),
		DrainPath: filepath.Join(stateDir, "drain.json"), LogPath: filepath.Join(stateDir, "service.log"),
	}
	if err := vcsbroker.WriteServiceState(state.StatePath, state); err != nil {
		t.Fatal(err)
	}
	current := vcsBrokerServiceConfig{
		OwnerID: state.OwnerID, Profile: "example", Backend: "colima", GuestHome: "/home/guest",
		Specs: []broker.SessionSpec{currentSpec}, Workspace: currentWorkspace, ConfigHash: currentHash,
		BuildID: state.BuildID, StatePath: state.StatePath, ConfigPath: state.ConfigPath,
		ReadyPath: state.ReadyPath, RepairPath: state.RepairPath, DrainPath: state.DrainPath,
		LogPath: state.LogPath, DrainWait: time.Second,
	}
	desired := current
	desired.Workspace.Ignore = []string{"generated/"}
	desired.ConfigHash, err = vcsBrokerConfigHash(desired.GuestHome, desired.Workspace, desired.Specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(desired.ConfigPath, desired); err != nil {
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
	maintenance <- time.Now()
	if status := vcsbroker.ProbeHost(server.Port(), "reload-token"); status != vcsbroker.HostProbeHealthy {
		t.Fatalf("broker stopped serving before the active command finished: %v", status)
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
	maintenance <- time.Now()
	select {
	case token := <-deployed:
		if token != "reload-token" {
			t.Fatalf("reload changed token to %q", token)
		}
	case <-time.After(time.Second):
		t.Fatal("idle reload did not refresh guest shims")
	}
	var after vcsbroker.ServiceState
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		after, err = vcsbroker.ReadServiceState(state.StatePath)
		if err == nil && after.ConfigHash == desired.ConfigHash {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if after.ConfigHash != desired.ConfigHash || after.Token != state.Token || after.BrokerPID != state.BrokerPID {
		t.Fatalf("idle reload state = %#v (read error %v)", after, err)
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

func TestVCSBrokerBuildIdentityMismatchRestartsWhenIdle(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "upgrade", "colima", profile); err != nil {
		t.Fatal(err)
	}
	before := readVCSServiceState(t, manager, "upgrade")
	manager.buildID = "replacement-build"
	if err := manager.ensure(vcsTestBackend(), "upgrade", "colima", profile); err != nil {
		t.Fatal(err)
	}
	after := readVCSServiceState(t, manager, "upgrade")
	if starts, stops := runtime.counts(); starts != 1 || stops != 0 || runtime.restartCount() != 1 {
		t.Fatalf("build replacement starts=%d stops=%d requests=%d, want 1, 0, 1", starts, stops, runtime.restartCount())
	}
	if before != after {
		t.Fatalf("ensure replaced the daemon before it became idle: before=%#v after=%#v", before, after)
	}
}

func TestVCSBrokerDaemonPerformsBuildReplacementOnlyAfterIdlePause(t *testing.T) {
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
		OwnerID: "build-owner", BrokerPID: os.Getpid(), TunnelPID: 201,
		HostPort: server.Port(), GuestPort: vcsBrokerGuestPort, Token: "old-token",
		ConfigHash: hash, BuildID: "old-build", TunnelTarget: "vm.test",
		StatePath: store.StatePath, ConfigPath: filepath.Join(stateDir, "service.json"),
		ReadyPath: filepath.Join(stateDir, "ready.json"), RepairPath: filepath.Join(stateDir, "repair.json"),
		DrainPath: filepath.Join(stateDir, "drain.json"), LogPath: filepath.Join(stateDir, "service.log"),
	}
	if err := vcsbroker.WriteServiceState(state.StatePath, state); err != nil {
		t.Fatal(err)
	}
	current := vcsBrokerServiceConfig{
		OwnerID: state.OwnerID, Profile: "example", Backend: "colima", GuestHome: "/home/guest",
		Specs: []broker.SessionSpec{spec}, Workspace: workspace, ConfigHash: hash, BuildID: state.BuildID,
		StatePath: state.StatePath, ConfigPath: state.ConfigPath, ReadyPath: state.ReadyPath,
		RepairPath: state.RepairPath, DrainPath: state.DrainPath, LogPath: state.LogPath, DrainWait: time.Second,
	}
	desired := current
	desired.BuildID = "new-build"
	service := &runningVCSBrokerService{
		state: state, backend: &vm.MockBackend{}, server: server,
		tunnel: tunnel.ReverseForwardOwner{OwnerID: state.OwnerID, PID: state.TunnelPID, HostPort: state.HostPort, GuestPort: state.GuestPort, Target: state.TunnelTarget},
	}
	if !server.TryPauseIfIdle() {
		t.Fatal("idle daemon could not enter its replacement pause")
	}
	previousStart := startVCSBrokerReplacementFn
	previousStop := stopVCSBrokerTunnelFn
	started := 0
	startVCSBrokerReplacementFn = func(cfg vcsBrokerServiceConfig) (vcsbroker.ServiceState, error) {
		started++
		replacement := state
		replacement.BrokerPID++
		replacement.TunnelPID++
		replacement.Token = "new-token"
		replacement.BuildID = cfg.BuildID
		if err := vcsbroker.WriteServiceState(cfg.StatePath, replacement); err != nil {
			return vcsbroker.ServiceState{}, err
		}
		return replacement, nil
	}
	stopVCSBrokerTunnelFn = func(string, string, tunnel.ReverseForwardOwner) bool { return true }
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
		OwnerID: "stale-owner", BrokerPID: process.Process.Pid, TunnelPID: process.Process.Pid,
		HostPort: 34567, GuestPort: vcsBrokerGuestPort, Token: "stale-token",
		ConfigHash: "stale-hash", BuildID: "stale-build", TunnelTarget: "vm.test",
		StatePath: t.TempDir() + "/state", ConfigPath: t.TempDir() + "/config", ReadyPath: t.TempDir() + "/ready",
		RepairPath: t.TempDir() + "/repair", DrainPath: t.TempDir() + "/drain", LogPath: t.TempDir() + "/log",
	}
	backend := &vm.MockBackend{SSHScriptOut: "__CLVCS[204]CLVCS__"}
	if vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID) {
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
	if len(backend.SSHScriptCalls) != 1 || !strings.Contains(backend.SSHScriptCalls[0].Script, "stale-owner") {
		t.Fatalf("stale owner guest cleanup calls = %#v", backend.SSHScriptCalls)
	}
}

func TestVCSBrokerVMDeathRequiresThreeConsecutiveChecks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := vcsbroker.NewStateStore(home, "example", time.Second)
	statePath := store.StatePath
	state := vcsbroker.ServiceState{OwnerID: "vm-owner", StatePath: statePath}
	if err := vcsbroker.WriteServiceState(statePath, state); err != nil {
		t.Fatal(err)
	}
	service := &runningVCSBrokerService{
		state:   state,
		backend: &vm.MockBackend{RunningProfiles: map[string]bool{"example": false}},
	}
	cfg := vcsBrokerServiceConfig{
		OwnerID: "vm-owner", Profile: "example", StatePath: statePath,
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
	state := vcsbroker.ServiceState{OwnerID: "transient-owner", StatePath: store.StatePath}
	if err := vcsbroker.WriteServiceState(store.StatePath, state); err != nil {
		t.Fatal(err)
	}
	backend := &runningSequenceBackend{values: []bool{false, false, true, false, false, false}}
	service := &runningVCSBrokerService{state: state, backend: backend}
	cfg := vcsBrokerServiceConfig{
		OwnerID: state.OwnerID, Profile: "transient", StatePath: store.StatePath,
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
	state := vcsbroker.ServiceState{OwnerID: "locked-owner", StatePath: store.StatePath}
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
		OwnerID: state.OwnerID, Profile: "locked-death", StatePath: state.StatePath,
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
		"vcs-broker", "serve", "--owner", "hard-owner")
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
	if childPID == 0 || !vcsBrokerProcessMatches(process.Process.Pid, "hard-owner") {
		t.Fatalf("test broker process was not identifiable: daemon=%d child=%d", process.Process.Pid, childPID)
	}
	if err := stopVCSBrokerProcess(process.Process.Pid, "hard-owner"); err != nil {
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
	if err := syscall.Kill(daemon.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = daemon.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, _ := exec.Command("ps", "-p", strconv.Itoa(childPID), "-o", "stat=").Output()
		state := strings.TrimSpace(string(output))
		if state == "" || strings.HasPrefix(state, "Z") {
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
	daemon := exec.Command(os.Args[0], "-test.run", "^TestVCSBrokerSubprocessHelper$")
	daemon.Env = append(os.Environ(),
		"CLOISTER_VCS_HELPER=drain-daemon",
		"CLOISTER_VCS_REPO="+repo,
		"CLOISTER_VCS_PORT="+portPath,
		"CLOISTER_VCS_POST="+postPath,
		"CLOISTER_VCS_RELEASE="+releasePath,
		"CLOISTER_VCS_REFRESH="+refreshPath,
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
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && vcsbroker.ProbeHost(port, "detached-token") != vcsbroker.HostProbeDraining {
		time.Sleep(time.Millisecond)
	}
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
		if err := os.WriteFile(os.Getenv("CLOISTER_VCS_PORT"), []byte(strconv.Itoa(server.Port())+"\n"), 0o600); err != nil {
			os.Exit(7)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		<-signals
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = server.Drain(ctx)
		cancel()
		if err != nil {
			os.Exit(8)
		}
		os.Exit(0)
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
