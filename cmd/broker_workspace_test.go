package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/vm"
	"cloister.io/internal/workspace"
)

type commandReconcilerBroker struct {
	broker.Mock
	reconcileCalls int
	events         []string
}

func (b *commandReconcilerBroker) ReconcileProfile(context.Context, string, []broker.SessionSpec) error {
	b.reconcileCalls++
	b.events = append(b.events, "reconcile")
	return nil
}

func (b *commandReconcilerBroker) Create(ctx context.Context, spec broker.SessionSpec) error {
	b.events = append(b.events, "create")
	return b.Mock.Create(ctx, spec)
}

func TestStartVMSelectsBrokerProviderAndFlushes(t *testing.T) {
	root := t.TempDir()
	backend := &vm.MockBackend{SSHAccessVal: vm.SSHAccess{Host: "vm.local", User: "guest"}}
	syncBroker := &broker.Mock{}
	restoreBrokerFactory(t, syncBroker, nil)
	p := &config.Profile{
		StartDir: root,
		Workspace: config.WorkspaceConfig{
			Mode: config.WorkspaceModeBroker,
		},
		MountPolicy: config.ResourcePolicy{IsSet: true, Mode: "none"},
	}

	if err := startVM(backend, "work", p, nil, false); err != nil {
		t.Fatal(err)
	}
	if len(backend.StartSpecs) != 1 {
		t.Fatalf("start specs = %d", len(backend.StartSpecs))
	}
	spec := backend.StartSpecs[0]
	if spec.WorkspaceProvider != vm.BrokerWorkspace || spec.WorkspaceMount != nil {
		t.Fatalf("StartSpec = %#v", spec)
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
}

func TestStartVMBrokerDependencyFailurePreventsVMStart(t *testing.T) {
	backend := &vm.MockBackend{}
	restoreBrokerFactory(t, nil, errors.New("Mutagen missing, install it"))
	p := &config.Profile{StartDir: t.TempDir(), Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}}
	err := startVM(backend, "work", p, nil, false)
	if err == nil || !strings.Contains(err.Error(), "install") {
		t.Fatalf("startVM() error = %v", err)
	}
	if len(backend.StartCalls) != 0 {
		t.Fatalf("VM started despite broker dependency failure: %v", backend.StartCalls)
	}
}

func TestStartVMWithWorkspaceReconcilesFullCollectionBeforeActivation(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"apps/api", "tools/cli"} {
		if err := os.MkdirAll(filepath.Join(root, project), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	backend := &vm.MockBackend{SSHAccessVal: vm.SSHAccess{Host: "vm.local", User: "guest"}}
	syncBroker := &commandReconcilerBroker{}
	restoreBrokerFactory(t, syncBroker, nil)
	profile := &config.Profile{
		StartDir: root,
		Workspace: config.WorkspaceConfig{
			Mode: config.WorkspaceModeWorkspace,
			Root: root,
		},
		MountPolicy: config.ResourcePolicy{IsSet: true, Mode: "none"},
	}

	if err := startVMWithWorkspace(backend, "local-dev", profile, nil, vm.WorkspaceBroker, "", false); err != nil {
		t.Fatal(err)
	}
	if syncBroker.reconcileCalls != 1 {
		t.Fatalf("full collection reconcile calls = %d, want 1", syncBroker.reconcileCalls)
	}
	if len(syncBroker.events) == 0 || syncBroker.events[0] != "reconcile" {
		t.Fatalf("activation order = %v, want reconciliation first", syncBroker.events)
	}
	if got := countBrokerCalls(syncBroker.Calls, broker.OperationCreate); got != 2 {
		t.Fatalf("create calls = %d, want 2", got)
	}
}

func TestStartVMAtPathActivatesOnePolicyParitySessionWithoutReconciliation(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "apps", "api")
	sibling := filepath.Join(root, "tools", "cli")
	for _, path := range []string{project, sibling} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	backend := &vm.MockBackend{SSHAccessVal: vm.SSHAccess{Host: "vm.local", User: "guest"}}
	syncBroker := &commandReconcilerBroker{}
	restoreBrokerFactory(t, syncBroker, nil)
	profile := &config.Profile{
		StartDir: root,
		Workspace: config.WorkspaceConfig{
			Mode:          config.WorkspaceModeWorkspace,
			Root:          root,
			Ignore:        []string{"tmp/"},
			ProjectIgnore: map[string][]string{"apps/api": {".local-generated/"}},
		},
		MountPolicy: config.ResourcePolicy{IsSet: true, Mode: "none"},
	}
	want, err := brokerSessionSpecAtPath(backend, "local-dev", profile, project)
	if err != nil {
		t.Fatal(err)
	}

	if err := startVMAtPath(backend, "local-dev", profile, nil, project, false); err != nil {
		t.Fatal(err)
	}
	if syncBroker.reconcileCalls != 0 {
		t.Fatalf("path-specific cold open reconciled %d time(s)", syncBroker.reconcileCalls)
	}
	created := brokerCallSpecs(syncBroker.Calls, broker.OperationCreate)
	if len(created) != 1 || !reflect.DeepEqual(created[0], *want) {
		t.Fatalf("created specs = %#v, want %#v", created, *want)
	}
}

func TestStartVMLegacyBrokerActivatesOneWithoutReconciliation(t *testing.T) {
	root := t.TempDir()
	backend := &vm.MockBackend{SSHAccessVal: vm.SSHAccess{Host: "vm.local", User: "guest"}}
	syncBroker := &commandReconcilerBroker{}
	restoreBrokerFactory(t, syncBroker, nil)
	profile := &config.Profile{
		StartDir:    root,
		Workspace:   config.WorkspaceConfig{Mode: config.WorkspaceModeBroker},
		MountPolicy: config.ResourcePolicy{IsSet: true, Mode: "none"},
	}

	if err := startVM(backend, "local-dev", profile, nil, false); err != nil {
		t.Fatal(err)
	}
	if syncBroker.reconcileCalls != 0 {
		t.Fatalf("legacy broker reconciled %d time(s)", syncBroker.reconcileCalls)
	}
	if got := countBrokerCalls(syncBroker.Calls, broker.OperationCreate); got != 1 {
		t.Fatalf("create calls = %d, want 1", got)
	}
}

func TestStopVMBrokerConflictLeavesVMRunning(t *testing.T) {
	backend := &vm.MockBackend{SSHAccessVal: vm.SSHAccess{Host: "vm.local", User: "guest"}}
	syncBroker := &broker.Mock{StatusValue: broker.Status{State: broker.StateProblem, ConflictCount: 1}}
	restoreBrokerFactory(t, syncBroker, nil)
	p := &config.Profile{StartDir: t.TempDir(), Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeBroker}}
	err := stopVM(backend, "work", p, false, false)
	if err == nil || !strings.Contains(err.Error(), "unresolved conflict") {
		t.Fatalf("stopVM() error = %v", err)
	}
	if len(backend.StopCalls) != 0 {
		t.Fatalf("backend stop calls = %v", backend.StopCalls)
	}
}

func TestBrokerSessionSpecAtPathUsesWorkspacePolicyForOneProject(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"apps/api", "tools/scraper"} {
		if err := os.MkdirAll(filepath.Join(root, project), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	backend := &vm.MockBackend{SSHAccessVal: vm.SSHAccess{Host: "vm.local", User: "guest"}}
	p := &config.Profile{
		StartDir: root,
		Workspace: config.WorkspaceConfig{
			Mode:          config.WorkspaceModeWorkspace,
			Root:          root,
			Ignore:        []string{"tmp/"},
			ProjectIgnore: map[string][]string{"apps/api": {".local-generated/"}},
		},
	}

	spec, err := brokerSessionSpecAtPath(backend, "work", p, filepath.Join(root, "apps", "api"))
	if err != nil {
		t.Fatal(err)
	}
	collection, err := brokerSessionSpecs(backend, "work", p)
	if err != nil {
		t.Fatal(err)
	}
	var want broker.SessionSpec
	for _, candidate := range collection {
		if filepath.Base(candidate.HostRoot) == "api" {
			want = candidate
		}
	}
	if !reflect.DeepEqual(*spec, want) {
		t.Fatalf("explicit project spec = %#v, want %#v", *spec, want)
	}
	if spec.MaxEntries != workspace.DefaultMaxEntryCount || spec.MaxStagingFileSize != workspace.DefaultMaxStagingFileSize {
		t.Fatalf("guardrails = %d/%q", spec.MaxEntries, spec.MaxStagingFileSize)
	}
	if spec.ProbeMode != "assume" || !spec.SkipGitignores || len(spec.MandatoryIgnore) == 0 {
		t.Fatalf("workspace policy not applied: %#v", *spec)
	}
	if !hasIgnore(spec.Ignore, "tmp/") || !hasIgnore(spec.Ignore, ".local-generated/") {
		t.Fatalf("ignores = %v", spec.Ignore)
	}
}

func TestBrokerSessionSpecAtPathRejectsProjectOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "apps/api"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &vm.MockBackend{}
	p := &config.Profile{
		StartDir:  root,
		Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeWorkspace, Root: root},
	}

	_, err := brokerSessionSpecAtPath(backend, "work", p, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "outside the workspace root") {
		t.Fatalf("brokerSessionSpecAtPath() error = %v", err)
	}
}

func TestBrokerSessionSpecAtPathKeepsSingleProjectBrokerBehavior(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "service")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &vm.MockBackend{SSHAccessVal: vm.SSHAccess{Host: "vm.local", User: "guest"}}
	p := &config.Profile{
		StartDir:  root,
		Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeBroker, Ignore: []string{"tmp/"}},
	}

	spec, err := brokerSessionSpecAtPath(backend, "work", p, project)
	if err != nil {
		t.Fatal(err)
	}
	want, err := broker.BuildSessionSpec("work", project, backend.SSHConfig("work"), p.Workspace.Ignore)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*spec, want) {
		t.Fatalf("single-project broker spec = %#v, want %#v", *spec, want)
	}
}

func TestEnsureBrokerWorkspaceAtPathDoesNotReconcileCollectionSiblings(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "apps", "api")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &vm.MockBackend{SSHAccessVal: vm.SSHAccess{Host: "vm.local", User: "guest"}}
	syncBroker := &commandReconcilerBroker{}
	restoreBrokerFactory(t, syncBroker, nil)
	profile := &config.Profile{
		StartDir: root,
		Workspace: config.WorkspaceConfig{
			Mode: config.WorkspaceModeWorkspace,
			Root: root,
		},
	}

	if err := ensureBrokerWorkspaceAtPath(backend, "local-dev", profile, project); err != nil {
		t.Fatal(err)
	}
	if syncBroker.reconcileCalls != 0 {
		t.Fatalf("single-path open reconciled collection %d time(s)", syncBroker.reconcileCalls)
	}
}

func hasIgnore(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countBrokerCalls(calls []broker.Call, operation broker.Operation) int {
	return len(brokerCallSpecs(calls, operation))
}

func brokerCallSpecs(calls []broker.Call, operation broker.Operation) []broker.SessionSpec {
	var specs []broker.SessionSpec
	for _, call := range calls {
		if call.Operation == operation {
			specs = append(specs, call.Spec)
		}
	}
	return specs
}

func restoreBrokerFactory(t *testing.T, value broker.SyncBroker, err error) {
	t.Helper()
	previous := newWorkspaceBroker
	newWorkspaceBroker = func() (broker.SyncBroker, error) { return value, err }
	t.Cleanup(func() { newWorkspaceBroker = previous })
}
