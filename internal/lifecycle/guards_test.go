// Proprietary and confidential. All rights reserved.

package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloister.io/internal/broker"
	"cloister.io/internal/vm"
)

type fakeSysctl map[string]uint64

func (f fakeSysctl) ReadUint64(name string) (uint64, error) {
	value, ok := f[name]
	if !ok {
		return 0, errors.New("missing sysctl")
	}
	return value, nil
}

type countingSysctl struct {
	reads int
}

func (s *countingSysctl) ReadUint64(name string) (uint64, error) {
	s.reads++
	if name == "kern.num_files" {
		return 100, nil
	}
	return 1_000_000, nil
}

type staleBackend struct {
	vm.MockBackend
	attempts int
}

func (b *staleBackend) Start(profile string, spec vm.StartSpec) error {
	b.attempts++
	b.StartCalls = append(b.StartCalls, profile)
	b.StartSpecs = append(b.StartSpecs, spec)
	if b.attempts == 1 {
		return errors.New("disk locked")
	}
	return nil
}

func (b *staleBackend) DiagnoseStartFailure(string) *vm.StaleLockDiagnosis {
	return &vm.StaleLockDiagnosis{Summary: "stale"}
}

func (b *staleBackend) ClearStaleLock(string) (int, error) {
	return 1, nil
}

func TestCheckFDHeadroom(t *testing.T) {
	policy := DefaultFDPolicy()
	cases := []struct {
		name       string
		used       uint64
		limit      uint64
		wantWarn   bool
		wantRefuse bool
	}{
		{name: "healthy", used: 500_000, limit: 1_000_000},
		{name: "warn ratio", used: 700_000, limit: 1_000_000, wantWarn: true},
		{name: "refuse ratio", used: 850_000, limit: 1_000_000, wantWarn: true, wantRefuse: true},
		{name: "warn headroom", used: 205_000, limit: 300_000, wantWarn: true},
		{name: "refuse headroom", used: 250_001, limit: 300_000, wantWarn: true, wantRefuse: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckFDHeadroom(fakeSysctl{"kern.num_files": tc.used, "kern.maxfiles": tc.limit}, policy)
			if err != nil {
				t.Fatal(err)
			}
			if (got.Warning != "") != tc.wantWarn || got.Refuse != tc.wantRefuse {
				t.Fatalf("warning=%q refuse=%v, want warn=%v refuse=%v", got.Warning, got.Refuse, tc.wantWarn, tc.wantRefuse)
			}
		})
	}
}

func TestCheckFDHeadroomReaderFailure(t *testing.T) {
	_, err := CheckFDHeadroom(fakeSysctl{}, DefaultFDPolicy())
	if err == nil {
		t.Fatal("expected sysctl failure")
	}
}

func TestCheckWorkspaceRefusesBroadVirtiofsRoot(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(root, project, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, err := CheckWorkspace(root, vm.VirtiofsWorkspace, WorkspacePolicy{WarnEntries: 2, RefuseEntries: 100, ProjectChildLimit: 2})
	if err == nil || !strings.Contains(err.Error(), "child Git projects") {
		t.Fatalf("CheckWorkspace() error = %v, want multi-project refusal", err)
	}
}

func TestCheckWorkspaceEntryThresholds(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy := WorkspacePolicy{WarnEntries: 2, RefuseEntries: 3, ProjectChildLimit: 2}
	if _, err := CheckWorkspace(root, vm.VirtiofsWorkspace, policy); err == nil {
		t.Fatal("expected entry-count refusal")
	}
	assessment, err := CheckWorkspace(root, vm.BrokerWorkspace, policy)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Warning == "" {
		t.Fatal("expected broad routing-root warning for broker mode")
	}
}

func TestCheckWorkspaceBrokerRequiresSingleProjectRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "child", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := CheckWorkspace(root, vm.BrokerWorkspace, DefaultWorkspacePolicy())
	if err == nil || !strings.Contains(err.Error(), "one explicit project root") {
		t.Fatalf("CheckWorkspace() error = %v", err)
	}
	if _, err := CheckWorkspace(filepath.Join(root, "child"), vm.BrokerWorkspace, DefaultWorkspacePolicy()); err != nil {
		t.Fatalf("explicit project root rejected: %v", err)
	}
}

func TestCoordinatorWiresStartSpec(t *testing.T) {
	root := t.TempDir()
	backend := &vm.MockBackend{}
	var stderr bytes.Buffer
	coordinator := NewCoordinator(backend)
	coordinator.GOOS = "linux"
	coordinator.Stderr = &stderr
	err := coordinator.Start(StartRequest{
		Profile:            "work",
		CPUs:               4,
		MemoryGB:           8,
		DiskGB:             60,
		RootDiskGB:         20,
		MountInotify:       true,
		WorkspaceDir:       root,
		WorkspaceProvider:  vm.VirtiofsWorkspace,
		SupplementalMounts: []vm.Mount{{Location: "/keys"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.StartSpecs) != 1 {
		t.Fatalf("got %d starts, want 1", len(backend.StartSpecs))
	}
	spec := backend.StartSpecs[0]
	if spec.WorkspaceMount == nil || spec.WorkspaceMount.Location != root || len(spec.SupplementalMounts) != 1 || !spec.MountInotify {
		t.Fatalf("unexpected StartSpec: %#v", spec)
	}
}

func TestCoordinatorRefusesLowFDHeadroom(t *testing.T) {
	backend := &vm.MockBackend{}
	coordinator := NewCoordinator(backend)
	coordinator.GOOS = "darwin"
	coordinator.Sysctl = fakeSysctl{"kern.num_files": 900_000, "kern.maxfiles": 1_000_000}
	coordinator.Stderr = &bytes.Buffer{}
	err := coordinator.Start(StartRequest{
		Profile:           "work",
		WorkspaceDir:      t.TempDir(),
		WorkspaceProvider: vm.VirtiofsWorkspace,
	})
	if err == nil || !strings.Contains(err.Error(), "file descriptor guard refused") {
		t.Fatalf("Coordinator.Start() error = %v, want FD refusal", err)
	}
	if len(backend.StartCalls) != 0 {
		t.Fatalf("backend started %d times after FD refusal", len(backend.StartCalls))
	}
}

func TestCoordinatorRerunsGuardsAfterStaleLockRecovery(t *testing.T) {
	backend := &staleBackend{}
	sysctl := &countingSysctl{}
	coordinator := NewCoordinator(backend)
	coordinator.GOOS = "darwin"
	coordinator.Sysctl = sysctl
	coordinator.Stderr = &bytes.Buffer{}
	coordinator.Recover = func(recoverer vm.StaleLockRecoverer, profile string, diagnosis *vm.StaleLockDiagnosis) error {
		_, err := recoverer.ClearStaleLock(profile)
		return err
	}
	if err := coordinator.Start(StartRequest{
		Profile:           "work",
		WorkspaceDir:      t.TempDir(),
		WorkspaceProvider: vm.VirtiofsWorkspace,
	}); err != nil {
		t.Fatal(err)
	}
	if backend.attempts != 2 {
		t.Fatalf("backend attempts = %d, want 2", backend.attempts)
	}
	if sysctl.reads != 4 {
		t.Fatalf("sysctl reads = %d, want 4 from two complete guard passes", sysctl.reads)
	}
}

func TestCoordinatorBrokerStartCreatesAndFlushesWithoutWorkspaceMount(t *testing.T) {
	root := t.TempDir()
	backend := &vm.MockBackend{}
	syncBroker := &broker.Mock{}
	spec, err := broker.BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	coordinator.GOOS = "linux"
	coordinator.Stderr = &bytes.Buffer{}
	err = coordinator.Start(StartRequest{
		Profile:           "work",
		WorkspaceDir:      root,
		WorkspaceProvider: vm.BrokerWorkspace,
		MountInotify:      true,
		BrokerSpec:        &spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.StartSpecs) != 1 {
		t.Fatalf("start specs = %d", len(backend.StartSpecs))
	}
	start := backend.StartSpecs[0]
	if start.WorkspaceProvider != vm.BrokerWorkspace || start.WorkspaceMount != nil || start.MountInotify {
		t.Fatalf("broker StartSpec = %#v", start)
	}
	want := []broker.Operation{broker.OperationStatus, broker.OperationCreate, broker.OperationFlush, broker.OperationStatus}
	if len(syncBroker.Calls) != len(want) {
		t.Fatalf("broker calls = %#v", syncBroker.Calls)
	}
	for i := range want {
		if syncBroker.Calls[i].Operation != want[i] {
			t.Fatalf("call %d = %s, want %s", i, syncBroker.Calls[i].Operation, want[i])
		}
	}
	if len(backend.SSHCommandCalls) != 1 || !strings.Contains(backend.SSHCommandCalls[0].Command, "$HOME/workspaces/") {
		t.Fatalf("guest root command = %#v", backend.SSHCommandCalls)
	}
}

func TestCoordinatorStopRefusesConflictBeforeBackendStop(t *testing.T) {
	backend := &vm.MockBackend{}
	syncBroker := &broker.Mock{StatusValue: broker.Status{State: broker.StateProblem, ConflictCount: 1}}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	spec := &broker.SessionSpec{Profile: "work", Name: "cloister-work-id"}
	err := coordinator.Stop(context.Background(), "work", spec, false, false)
	if err == nil || !strings.Contains(err.Error(), "unresolved conflict") {
		t.Fatalf("Stop() error = %v", err)
	}
	if len(backend.StopCalls) != 0 {
		t.Fatalf("backend stopped despite conflict: %v", backend.StopCalls)
	}
}

func TestCoordinatorDestructiveStopTerminatesAfterCleanPause(t *testing.T) {
	backend := &vm.MockBackend{}
	syncBroker := &broker.Mock{}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	spec := &broker.SessionSpec{Profile: "work", Name: "cloister-work-id"}
	if err := coordinator.Stop(context.Background(), "work", spec, true, false); err != nil {
		t.Fatal(err)
	}
	want := []broker.Operation{broker.OperationFlush, broker.OperationStatus, broker.OperationPause, broker.OperationTerminate}
	for i := range want {
		if syncBroker.Calls[i].Operation != want[i] {
			t.Fatalf("call %d = %s, want %s", i, syncBroker.Calls[i].Operation, want[i])
		}
	}
	if len(backend.StopCalls) != 1 {
		t.Fatalf("backend stop calls = %v", backend.StopCalls)
	}
}
