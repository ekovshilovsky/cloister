package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloister.io/internal/config"
	"cloister.io/internal/vcsbroker"
	"cloister.io/internal/vm"
)

type fakePersistentVCSRuntime struct {
	mu              sync.Mutex
	starts          int
	stops           []vcsbroker.ServiceState
	brokerAlive     bool
	tunnelAlive     bool
	endpointHealthy bool
}

type mismatchedPublishRuntime struct {
	*fakePersistentVCSRuntime
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

func (f *fakePersistentVCSRuntime) Healthy(_ vm.Backend, _ string, _ vcsbroker.ServiceState) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.brokerAlive && f.tunnelAlive && f.endpointHealthy
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
		ConfigHash: cfg.ConfigHash, TunnelTarget: "vm.test",
		ConfigPath: cfg.ConfigPath, ReadyPath: cfg.ReadyPath, LogPath: cfg.LogPath,
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

func (f *fakePersistentVCSRuntime) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, len(f.stops)
}

func newPersistentVCSTest(t *testing.T) (*vcsBrokerManager, *fakePersistentVCSRuntime, *config.Profile) {
	t.Helper()
	runtime := newFakePersistentVCSRuntime()
	var sequence atomic.Int64
	manager := &vcsBrokerManager{
		stateDir: t.TempDir(), lockWait: 500 * time.Millisecond, runtime: runtime,
		newID: func() (string, error) { return fmt.Sprintf("request-%d", sequence.Add(1)), nil },
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
	runtime.mu.Lock()
	runtime.endpointHealthy = false
	runtime.mu.Unlock()
	if err := manager.ensure(vcsTestBackend(), "tunnel", "colima", profile); err != nil {
		t.Fatal(err)
	}
	if starts, stops := runtime.counts(); starts != 2 || stops != 1 {
		t.Fatalf("dead tunnel recovery starts=%d stops=%d, want 2 and 1", starts, stops)
	}
}

func TestVCSBrokerWorkspaceConfigHashChangeRestartsService(t *testing.T) {
	manager, runtime, profile := newPersistentVCSTest(t)
	if err := manager.ensure(vcsTestBackend(), "changed", "colima", profile); err != nil {
		t.Fatal(err)
	}
	before := readVCSServiceState(t, manager, "changed")
	profile.Workspace.Ignore = []string{"generated/"}
	if err := manager.ensure(vcsTestBackend(), "changed", "colima", profile); err != nil {
		t.Fatal(err)
	}
	after := readVCSServiceState(t, manager, "changed")
	if starts, stops := runtime.counts(); starts != 2 || stops != 1 {
		t.Fatalf("config restart starts=%d stops=%d, want 2 and 1", starts, stops)
	}
	if before.ConfigHash == after.ConfigHash || before.Token == after.Token {
		t.Fatalf("changed mapper config did not publish a new service: before=%#v after=%#v", before, after)
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
		ConfigHash: "stale-hash", TunnelTarget: "vm.test",
		ConfigPath: t.TempDir() + "/config", ReadyPath: t.TempDir() + "/ready", LogPath: t.TempDir() + "/log",
	}
	backend := &vm.MockBackend{SSHScriptOut: "__CLVCS[204]CLVCS__"}
	if vcsBrokerProcessMatches(state.BrokerPID, state.OwnerID) {
		t.Fatal("unrelated process was accepted as broker owner")
	}
	if (realVCSBrokerRuntime{}).Healthy(backend, "stale", state) {
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
