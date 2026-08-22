// Proprietary and confidential. All rights reserved.

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/vm"
)

func TestOpenCommandWiresPathArgument(t *testing.T) {
	previous := runOpenPath
	var got string
	runOpenPath = func(path string) error {
		got = path
		return nil
	}
	t.Cleanup(func() { runOpenPath = previous })

	if err := openCmd.RunE(openCmd, []string{"./project"}); err != nil {
		t.Fatal(err)
	}
	if got != "./project" {
		t.Fatalf("open path = %q, want ./project", got)
	}
}

func TestOpenPathStartsActivatesEntersAndQuiescesBrokerProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scope := filepath.Join(home, "code")
	project := filepath.Join(scope, "project")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(home, ".cloister", "config.yaml")
	cfg := &config.Config{
		Profiles: map[string]*config.Profile{
			"work": {
				Backend:      "colima",
				StartDir:     scope,
				Workspace:    config.WorkspaceConfig{Mode: config.WorkspaceModeBroker},
				MountPolicy:  config.ResourcePolicy{IsSet: true, Mode: "none"},
				TunnelPolicy: config.ResourcePolicy{IsSet: true, Mode: "none"},
			},
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	backend := &vm.MockBackend{
		RunningProfiles: map[string]bool{"work": false},
		SSHAccessVal:    vm.SSHAccess{Host: "vm.local", User: "guest"},
	}
	previousResolver := resolveEnterBackend
	resolveEnterBackend = func(string) (vm.Backend, error) { return backend, nil }
	t.Cleanup(func() { resolveEnterBackend = previousResolver })

	syncBroker := &broker.Mock{}
	restoreBrokerFactory(t, syncBroker, nil)

	previousVCS := startVCSBrokerFn
	startVCSBrokerFn = func(vm.Backend, string, *config.Profile) (*vcsBrokerSession, error) { return nil, nil }
	t.Cleanup(func() { startVCSBrokerFn = previousVCS })

	if err := openPath(project); err != nil {
		t.Fatal(err)
	}
	if len(backend.StartCalls) != 1 || backend.StartCalls[0] != "work" {
		t.Fatalf("start calls = %v", backend.StartCalls)
	}
	if len(backend.StartSpecs) != 1 || backend.StartSpecs[0].WorkspaceProvider != vm.BrokerWorkspace || backend.StartSpecs[0].WorkspaceMount != nil {
		t.Fatalf("start specs = %#v", backend.StartSpecs)
	}
	if len(backend.SSHInteractiveCalls) != 1 || backend.SSHInteractiveCalls[0].Profile != "work" ||
		!strings.Contains(backend.SSHInteractiveCalls[0].Command, "$HOME/workspaces/project-") {
		t.Fatalf("interactive calls = %#v", backend.SSHInteractiveCalls)
	}

	wantOperations := []broker.Operation{
		// Activation: status, create, then FlushBroker (flush, status).
		broker.OperationStatus,
		broker.OperationCreate,
		broker.OperationFlush,
		broker.OperationStatus,
		// Quiesce: leading status (skip already-paused), FlushBroker (flush,
		// status), then pause.
		broker.OperationStatus,
		broker.OperationFlush,
		broker.OperationStatus,
		broker.OperationPause,
	}
	if len(syncBroker.Calls) != len(wantOperations) {
		t.Fatalf("broker calls = %#v", syncBroker.Calls)
	}
	canonicalProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range wantOperations {
		call := syncBroker.Calls[i]
		if call.Operation != want || call.Spec.HostRoot != canonicalProject {
			t.Fatalf("broker call %d = %#v, want operation %s at %q", i, call, want, canonicalProject)
		}
	}
}

func TestGuestShellAtQuotesVirtiofsPath(t *testing.T) {
	command, err := guestShellAt("/tmp/a'b")
	if err != nil {
		t.Fatal(err)
	}
	if command != `cd -- '/tmp/a'"'"'b' && exec "${SHELL:-/bin/bash}" -l` {
		t.Fatalf("guestShellAt() = %q", command)
	}
}
