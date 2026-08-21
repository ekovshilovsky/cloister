// Proprietary and confidential. All rights reserved.

package cmd

import (
	"errors"
	"strings"
	"testing"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/vm"
)

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

func restoreBrokerFactory(t *testing.T, value broker.SyncBroker, err error) {
	t.Helper()
	previous := newWorkspaceBroker
	newWorkspaceBroker = func() (broker.SyncBroker, error) { return value, err }
	t.Cleanup(func() { newWorkspaceBroker = previous })
}
