package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

type optionalReconcilerBroker struct {
	broker.Mock
	reconciled          bool
	requireBeforeCreate bool
	reconcileErr        error
	reconcileProfile    string
	reconcileDesired    []broker.SessionSpec
}

func (b *optionalReconcilerBroker) ReconcileProfile(_ context.Context, profile string, desired []broker.SessionSpec) error {
	b.reconciled = true
	b.reconcileProfile = profile
	b.reconcileDesired = append([]broker.SessionSpec(nil), desired...)
	return b.reconcileErr
}

func (b *optionalReconcilerBroker) Create(ctx context.Context, spec broker.SessionSpec) error {
	if b.requireBeforeCreate && !b.reconciled {
		return errors.New("create called before reconciliation")
	}
	return b.Mock.Create(ctx, spec)
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
		{name: "small table idle not refused", used: 3_000, limit: 49_152},
		{name: "small table high ratio refused", used: 42_000, limit: 49_152, wantWarn: true, wantRefuse: true},
		{name: "tiny table idle not refused", used: 1_500, limit: 24_576},
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

func TestCheckWorkspaceCollectionAllowsBroadRoutingRoot(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"apps/one", "tools/two"} {
		if err := os.MkdirAll(filepath.Join(root, project, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CheckWorkspace(root, vm.WorkspaceBroker, WorkspacePolicy{WarnEntries: 1, RefuseEntries: 1, ProjectChildLimit: 1}); err != nil {
		t.Fatalf("workspace collection routing root rejected: %v", err)
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
	if len(backend.SSHScriptCalls) != 1 || !strings.Contains(backend.SSHScriptCalls[0].Script, "$HOME/workspaces/") {
		t.Fatalf("guest root command = %#v", backend.SSHScriptCalls)
	}
}

func TestCoordinatorWorkspaceCollectionActivatesEverySession(t *testing.T) {
	root := t.TempDir()
	backend := &vm.MockBackend{}
	syncBroker := &optionalReconcilerBroker{requireBeforeCreate: true}
	var specs []broker.SessionSpec
	for _, name := range []string{"one", "two"} {
		project := filepath.Join(root, name)
		if err := os.MkdirAll(project, 0o700); err != nil {
			t.Fatal(err)
		}
		spec, err := broker.BuildSessionSpec("work", project, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		specs = append(specs, spec)
	}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	coordinator.GOOS = "linux"
	coordinator.Stderr = &bytes.Buffer{}
	err := coordinator.Start(StartRequest{
		Profile: "work", WorkspaceDir: root, WorkspaceProvider: vm.WorkspaceBroker,
		MountInotify: true, BrokerSpecs: specs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.StartSpecs) != 1 || backend.StartSpecs[0].WorkspaceProvider != vm.WorkspaceBroker || backend.StartSpecs[0].WorkspaceMount != nil || backend.StartSpecs[0].MountInotify {
		t.Fatalf("workspace StartSpec = %#v", backend.StartSpecs)
	}
	if !syncBroker.reconciled || syncBroker.reconcileProfile != "work" || !reflect.DeepEqual(syncBroker.reconcileDesired, specs) {
		t.Fatalf("reconciliation = called:%v profile:%q desired:%#v", syncBroker.reconciled, syncBroker.reconcileProfile, syncBroker.reconcileDesired)
	}
	want := []broker.Operation{
		broker.OperationStatus, broker.OperationCreate, broker.OperationFlush, broker.OperationStatus,
		broker.OperationStatus, broker.OperationCreate, broker.OperationFlush, broker.OperationStatus,
	}
	if len(syncBroker.Calls) != len(want) || len(backend.SSHScriptCalls) != 2 {
		t.Fatalf("broker calls = %#v, guest calls = %#v", syncBroker.Calls, backend.SSHScriptCalls)
	}
	for i := range want {
		if syncBroker.Calls[i].Operation != want[i] {
			t.Fatalf("call %d = %s, want %s", i, syncBroker.Calls[i].Operation, want[i])
		}
	}
}

func TestCoordinatorCollectionReconciliationFailurePreventsActivation(t *testing.T) {
	backend := &vm.MockBackend{}
	syncBroker := &optionalReconcilerBroker{reconcileErr: errors.New("ambiguous session list")}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	coordinator.GOOS = "linux"
	coordinator.Stderr = &bytes.Buffer{}
	spec := broker.SessionSpec{
		Profile: "local-dev", Name: "cloister-local-dev-111111111111111111111111",
		HostRoot: t.TempDir(), GuestRoot: "~/workspaces/example-111111111111",
	}

	err := coordinator.ActivateBrokers(context.Background(), []broker.SessionSpec{spec})
	if err == nil || !strings.Contains(err.Error(), "ambiguous session list") {
		t.Fatalf("ActivateBrokers() error = %v", err)
	}
	if len(syncBroker.Calls) != 0 || len(backend.SSHScriptCalls) != 0 {
		t.Fatalf("activation began after reconciliation failure: broker=%#v guest=%#v", syncBroker.Calls, backend.SSHScriptCalls)
	}
}

func TestExplicitEmptyBrokerSpecsRemainACompleteCollection(t *testing.T) {
	fallback := broker.SessionSpec{Name: "cloister-local-dev-111111111111111111111111"}
	request := StartRequest{
		BrokerSpec:  &fallback,
		BrokerSpecs: []broker.SessionSpec{},
	}

	if !hasCompleteBrokerCollection(request) {
		t.Fatal("explicit empty BrokerSpecs did not mark a complete collection")
	}
	if got := brokerSpecs(request); len(got) != 0 {
		t.Fatalf("brokerSpecs() = %#v, want explicit empty collection without fallback", got)
	}
}

func TestCoordinatorResetsGuestRootWhenSessionTargetDrifted(t *testing.T) {
	backend := &vm.MockBackend{}
	oldGuest := "~/workspaces/project-111111111111"
	newGuest := "~/workspaces/apps/api"
	syncBroker := &broker.Mock{StatusValue: broker.Status{State: broker.StateActive, GuestRoot: oldGuest}}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	coordinator.GOOS = "linux"
	coordinator.Stderr = &bytes.Buffer{}
	spec := broker.SessionSpec{
		Profile: "work", Name: "cloister-work-111111111111111111111111",
		HostRoot: t.TempDir(), GuestRoot: newGuest,
	}

	if err := coordinator.ActivateBroker(context.Background(), &spec); err != nil {
		t.Fatal(err)
	}
	if len(backend.SSHScriptCalls) != 1 || !strings.Contains(backend.SSHScriptCalls[0].Script, "rm -rf") {
		t.Fatalf("expected guest root reset on target drift, got %#v", backend.SSHScriptCalls)
	}
	if !strings.Contains(backend.SSHScriptCalls[0].Script, "workspaces/apps/api") {
		t.Fatalf("reset targeted the wrong guest root: %#v", backend.SSHScriptCalls)
	}
}

func TestCoordinatorAdoptsGuestRootWhenSessionTargetMatches(t *testing.T) {
	backend := &vm.MockBackend{}
	guest := "~/workspaces/apps/api"
	syncBroker := &broker.Mock{StatusValue: broker.Status{State: broker.StateActive, GuestRoot: guest}}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	coordinator.GOOS = "linux"
	coordinator.Stderr = &bytes.Buffer{}
	spec := broker.SessionSpec{
		Profile: "work", Name: "cloister-work-111111111111111111111111",
		HostRoot: t.TempDir(), GuestRoot: guest,
	}

	if err := coordinator.ActivateBroker(context.Background(), &spec); err != nil {
		t.Fatal(err)
	}
	if len(backend.SSHScriptCalls) != 1 || strings.Contains(backend.SSHScriptCalls[0].Script, "rm -rf") {
		t.Fatalf("matching guest root should be adopted, got %#v", backend.SSHScriptCalls)
	}
}

func TestCoordinatorSingleProjectActivationDoesNotReconcile(t *testing.T) {
	backend := &vm.MockBackend{}
	syncBroker := &optionalReconcilerBroker{}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	coordinator.GOOS = "linux"
	coordinator.Stderr = &bytes.Buffer{}
	spec := broker.SessionSpec{
		Profile: "local-dev", Name: "cloister-local-dev-111111111111111111111111",
		HostRoot: t.TempDir(), GuestRoot: "~/workspaces/example-111111111111",
	}

	if err := coordinator.ActivateBroker(context.Background(), &spec); err != nil {
		t.Fatal(err)
	}
	if syncBroker.reconciled {
		t.Fatal("single-project activation reconciled sibling sessions")
	}
}

// scriptedMutagenRunner replays recorded `mutagen sync list --long` output so
// activation can be exercised against the real status parser without a daemon.
type scriptedMutagenRunner struct {
	statuses   []scriptedStatus
	next       int
	operations []string
}

type scriptedStatus struct {
	output string
	err    error
}

type mutagenExitError int

func (e mutagenExitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e mutagenExitError) ExitCode() int { return int(e) }

func (r *scriptedMutagenRunner) Run(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
	r.operations = append(r.operations, strings.Join(args, " "))
	if len(args) >= 2 && args[0] == "sync" && args[1] == "list" {
		if r.next >= len(r.statuses) {
			return nil, fmt.Errorf("unscripted status call %d", r.next+1)
		}
		scripted := r.statuses[r.next]
		r.next++
		return []byte(scripted.output), scripted.err
	}
	return []byte("ok\n"), nil
}

func (r *scriptedMutagenRunner) ran(operation string) bool {
	for _, recorded := range r.operations {
		if strings.HasPrefix(recorded, operation) {
			return true
		}
	}
	return false
}

// scriptedSessionStatus renders the Mutagen 0.18.1 `sync list --long` shape for
// a connected session that is scanning files, plus any extra reported lines.
// Empty conflict lists are omitted the way Mutagen omits them.
func scriptedSessionStatus(name string, extra ...string) string {
	output := "Name: " + name + "\n" +
		"Alpha:\n\tConnected: Yes\nBeta:\n\tConnected: Yes\n"
	for _, line := range extra {
		output += line + "\n"
	}
	return output + "Status: Scanning files\n"
}

// A recreated session reports progress statuses right after the blocking flush
// returns. Progress must not roll activation back, while a real problem must.
func TestCoordinatorActivationSeparatesPostFlushProgressFromProblems(t *testing.T) {
	tests := []struct {
		name         string
		extraLines   []string
		wantErr      string
		wantRollback bool
	}{
		{name: "active progress"},
		{
			name:         "genuine problem",
			extraLines:   []string{"Last error: unable to stage files on beta"},
			wantErr:      "workspace is not clean",
			wantRollback: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			spec, err := broker.BuildSessionSpec("test-profile", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			missing := scriptedStatus{
				output: `Error: unable to locate requested sessions: specification "` + spec.Name + `" did not match any sessions`,
				err:    mutagenExitError(1),
			}
			runner := &scriptedMutagenRunner{statuses: []scriptedStatus{
				missing,
				missing,
				{output: scriptedSessionStatus(spec.Name, test.extraLines...)},
			}}
			syncBroker := &broker.Mutagen{
				Binary:  "mutagen",
				Runner:  runner,
				DataDir: filepath.Join(t.TempDir(), "data"),
				SSHDir:  filepath.Join(t.TempDir(), "ssh"),
				SSHPath: "/usr/bin/ssh",
				SCPPath: "/usr/bin/scp",
				Log:     &bytes.Buffer{},
			}
			coordinator := NewCoordinator(&vm.MockBackend{})
			coordinator.Broker = syncBroker
			coordinator.GOOS = "linux"
			coordinator.Stderr = &bytes.Buffer{}

			err = coordinator.ActivateBroker(context.Background(), &spec)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ActivateBroker() error = %v, want progress to be accepted", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ActivateBroker() error = %v, want %q", err, test.wantErr)
			}
			if !runner.ran("sync flush") {
				t.Fatalf("activation skipped the flush barrier: %v", runner.operations)
			}
			if got := runner.ran("sync pause"); got != test.wantRollback {
				t.Fatalf("rollback = %v, want %v: %v", got, test.wantRollback, runner.operations)
			}
		})
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
	want := []broker.Operation{broker.OperationStatus, broker.OperationFlush, broker.OperationStatus, broker.OperationPause, broker.OperationTerminate}
	if len(syncBroker.Calls) != len(want) {
		t.Fatalf("broker calls = %#v, want %v", syncBroker.Calls, want)
	}
	for i := range want {
		if syncBroker.Calls[i].Operation != want[i] {
			t.Fatalf("call %d = %s, want %s", i, syncBroker.Calls[i].Operation, want[i])
		}
	}
	if len(backend.StopCalls) != 1 {
		t.Fatalf("backend stop calls = %v", backend.StopCalls)
	}
}

func TestCoordinatorStopTreatsPausedSingleSessionAsQuiesced(t *testing.T) {
	backend := &vm.MockBackend{}
	syncBroker := &broker.Mock{StatusValue: broker.Status{State: broker.StatePaused, Description: "Paused"}}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	spec := &broker.SessionSpec{Profile: "work", Name: "cloister-work-id", HostRoot: "/project"}

	if err := coordinator.Stop(context.Background(), "work", spec, false, false); err != nil {
		t.Fatal(err)
	}
	want := []broker.Operation{broker.OperationStatus}
	if len(syncBroker.Calls) != len(want) || syncBroker.Calls[0].Operation != want[0] {
		t.Fatalf("broker calls = %#v, want status only", syncBroker.Calls)
	}
	if len(backend.StopCalls) != 1 {
		t.Fatalf("backend stop calls = %v", backend.StopCalls)
	}
}

func TestCoordinatorDestructiveStopTerminatesPausedWorkspaceSessions(t *testing.T) {
	backend := &vm.MockBackend{}
	syncBroker := &broker.Mock{StatusValue: broker.Status{State: broker.StatePaused, Description: "Paused"}}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	specs := []broker.SessionSpec{
		{Profile: "work", Name: "cloister-work-one", HostRoot: "/projects/one"},
		{Profile: "work", Name: "cloister-work-two", HostRoot: "/projects/two"},
	}

	if err := coordinator.StopBrokers(context.Background(), "work", specs, true, false); err != nil {
		t.Fatal(err)
	}
	want := []broker.Operation{
		broker.OperationStatus, broker.OperationStatus,
		broker.OperationTerminate, broker.OperationTerminate,
	}
	if len(syncBroker.Calls) != len(want) {
		t.Fatalf("broker calls = %#v, want %v", syncBroker.Calls, want)
	}
	for i := range want {
		if syncBroker.Calls[i].Operation != want[i] {
			t.Fatalf("call %d = %s, want %s", i, syncBroker.Calls[i].Operation, want[i])
		}
	}
	if len(backend.StopCalls) != 1 {
		t.Fatalf("backend stop calls = %v", backend.StopCalls)
	}
}

func TestCoordinatorStopRejectsUncleanPausedSession(t *testing.T) {
	backend := &vm.MockBackend{}
	syncBroker := &broker.Mock{StatusValue: broker.Status{
		State: broker.StatePaused, Description: "Paused", ConflictCount: 1,
	}}
	coordinator := NewCoordinator(backend)
	coordinator.Broker = syncBroker
	spec := &broker.SessionSpec{Profile: "work", Name: "cloister-work-id", HostRoot: "/project"}

	err := coordinator.Stop(context.Background(), "work", spec, false, false)
	if err == nil || !strings.Contains(err.Error(), "paused session is not clean") {
		t.Fatalf("Stop() error = %v", err)
	}
	if len(backend.StopCalls) != 0 {
		t.Fatalf("backend stopped despite paused conflict: %v", backend.StopCalls)
	}
}
