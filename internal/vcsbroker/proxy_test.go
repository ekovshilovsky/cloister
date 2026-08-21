// Proprietary and confidential. All rights reserved.

package vcsbroker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloister.io/internal/broker"
)

type runnerObservation struct {
	Executable       string
	Args             []string
	Dir              string
	BrokerCallsAtRun int
}

type recordingRunner struct {
	broker   *broker.Mock
	calls    []runnerObservation
	exitCode int
	err      error
}

func (r *recordingRunner) Run(_ context.Context, executable string, args []string, dir string, _ []string, output io.Writer) (int, error) {
	r.calls = append(r.calls, runnerObservation{
		Executable:       executable,
		Args:             append([]string(nil), args...),
		Dir:              dir,
		BrokerCallsAtRun: len(r.broker.Calls),
	})
	_, _ = io.WriteString(output, "host output\n")
	return r.exitCode, r.err
}

func TestMapperMapsGuestCWDToHostAndRejectsEscapes(t *testing.T) {
	hostRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostRoot, "services/api"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := testSpec(hostRoot)
	mapper, err := NewMapper("/home/dev", []broker.SessionSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := mapper.MapGuest("/home/dev/workspaces/project-123/services/api")
	if err != nil {
		t.Fatal(err)
	}
	host, err := mapper.ResolveHost(mapping)
	if err != nil {
		t.Fatal(err)
	}
	if host != filepath.Join(spec.HostRoot, "services/api") {
		t.Fatalf("host cwd = %q", host)
	}
	if _, err := mapper.MapGuest("/home/dev/workspaces/project-123/../other"); err == nil {
		t.Fatal("guest traversal escaped a mapped project")
	}
}

func TestProxyReadOnlyCommandsFlushBeforeOnly(t *testing.T) {
	for _, command := range []string{"status", "diff", "log", "branch"} {
		t.Run(command, func(t *testing.T) {
			proxy, mock, runner, cwd := testProxy(t)
			var output bytes.Buffer
			exit, err := proxy.Execute(context.Background(), Request{Tool: "git", CWD: cwd, Args: []string{command}}, &output)
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || output.String() != "host output\n" || len(runner.calls) != 1 {
				t.Fatalf("result exit=%d output=%q calls=%#v", exit, output.String(), runner.calls)
			}
			assertOperations(t, mock, broker.OperationFlush, broker.OperationStatus)
			if runner.calls[0].BrokerCallsAtRun != 2 {
				t.Fatalf("runner observed %d broker calls, want pre-flush and status", runner.calls[0].BrokerCallsAtRun)
			}
		})
	}
}

func TestProxyMutatingCommandsFlushBeforeAndAfter(t *testing.T) {
	commands := []string{"checkout", "reset", "merge", "pull", "stash", "rebase", "restore"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			proxy, mock, runner, cwd := testProxy(t)
			exit, err := proxy.Execute(context.Background(), Request{Tool: "git", CWD: cwd, Args: []string{command}}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || len(runner.calls) != 1 {
				t.Fatalf("result exit=%d calls=%#v", exit, runner.calls)
			}
			assertOperations(t, mock,
				broker.OperationFlush, broker.OperationStatus,
				broker.OperationFlush, broker.OperationStatus,
			)
			if runner.calls[0].BrokerCallsAtRun != 2 {
				t.Fatalf("host git ran after %d broker calls, want exactly the pre-flush and status", runner.calls[0].BrokerCallsAtRun)
			}
		})
	}
}

func TestProxyMutatingFlushesBeforeAndAfterNonzeroRun(t *testing.T) {
	proxy, mock, runner, cwd := testProxy(t)
	runner.exitCode = 1
	exit, err := proxy.Execute(context.Background(), Request{Tool: "git", CWD: cwd, Args: []string{"checkout", "missing"}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	assertOperations(t, mock, broker.OperationFlush, broker.OperationStatus, broker.OperationFlush, broker.OperationStatus)
	if runner.calls[0].BrokerCallsAtRun != 2 {
		t.Fatalf("runner ordering = %#v", runner.calls[0])
	}
}

func TestProxyRejectsCommandOutsideMappedWorkspace(t *testing.T) {
	proxy, mock, runner, _ := testProxy(t)
	exit, err := proxy.Execute(context.Background(), Request{
		Tool: "git", CWD: "/home/dev/other", Args: []string{"status"},
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "outside every registered workspace") {
		t.Fatalf("Execute() exit=%d error=%v", exit, err)
	}
	if len(mock.Calls) != 0 || len(runner.calls) != 0 {
		t.Fatalf("unmapped command reached broker or runner: broker=%#v runner=%#v", mock.Calls, runner.calls)
	}
}

func TestProxyPreservesHostGitExitCode(t *testing.T) {
	proxy, mock, runner, cwd := testProxy(t)
	runner.exitCode = 42
	exit, err := proxy.Execute(context.Background(), Request{
		Tool: "git", CWD: cwd, Args: []string{"status"},
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 42 {
		t.Fatalf("exit = %d, want host git exit 42", exit)
	}
	assertOperations(t, mock, broker.OperationFlush, broker.OperationStatus)
}

func TestProxySurfacesHostRunnerFailure(t *testing.T) {
	proxy, mock, runner, cwd := testProxy(t)
	runner.exitCode = 125
	runner.err = errors.New("exec transport failed")
	exit, err := proxy.Execute(context.Background(), Request{
		Tool: "git", CWD: cwd, Args: []string{"status"},
	}, io.Discard)
	if exit != 125 || err == nil || !strings.Contains(err.Error(), "exec transport failed") {
		t.Fatalf("Execute() exit=%d error=%v", exit, err)
	}
	assertOperations(t, mock, broker.OperationFlush, broker.OperationStatus)
}

func TestProxyEdgeCaseClassifications(t *testing.T) {
	cases := []struct {
		name       string
		tool       string
		args       []string
		env        []string
		operations []broker.Operation
	}{
		{
			name: "safe editor sentinel", tool: "git", args: []string{"commit"}, env: []string{"GIT_EDITOR=true"},
			operations: []broker.Operation{broker.OperationFlush, broker.OperationStatus, broker.OperationFlush, broker.OperationStatus},
		},
		{
			name: "push host credentials and hooks", tool: "git", args: []string{"push"},
			operations: []broker.Operation{broker.OperationFlush, broker.OperationStatus, broker.OperationFlush, broker.OperationStatus},
		},
		{
			name: "submodule update", tool: "git", args: []string{"submodule", "update"},
			operations: []broker.Operation{broker.OperationFlush, broker.OperationStatus, broker.OperationFlush, broker.OperationStatus},
		},
		{
			name: "gh passthrough", tool: "gh", args: []string{"pr", "view"},
			operations: []broker.Operation{broker.OperationFlush, broker.OperationStatus},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxy, mock, runner, cwd := testProxy(t)
			if _, err := proxy.Execute(context.Background(), Request{Tool: tc.tool, CWD: cwd, Args: tc.args, Env: tc.env}, io.Discard); err != nil {
				t.Fatal(err)
			}
			assertOperations(t, mock, tc.operations...)
			if len(runner.calls) != 1 || runner.calls[0].Executable != tc.tool || runner.calls[0].BrokerCallsAtRun != 2 {
				t.Fatalf("runner calls = %#v", runner.calls)
			}
		})
	}
}

func TestProxyMapsGitDashCAndAbsoluteFileArguments(t *testing.T) {
	proxy, _, runner, cwd := testProxy(t)
	// testProxy creates this directory and file below the registered root.
	guestFile := filepath.Join(cwd, "sub", "message.txt")
	_, err := proxy.Execute(context.Background(), Request{
		Tool: "git",
		CWD:  cwd,
		Args: []string{"-C", "sub", "commit", "-F", guestFile},
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	call := runner.calls[0]
	if filepath.Base(call.Dir) != "sub" || len(call.Args) != 3 || call.Args[0] != "commit" || !strings.HasPrefix(call.Args[2], filepath.Dir(call.Dir)) {
		t.Fatalf("mapped command = %#v", call)
	}
}

func TestProxyRejectsInteractiveAndHostExecutionOptions(t *testing.T) {
	proxy, _, runner, cwd := testProxy(t)
	for _, args := range [][]string{{"commit"}, {"rebase", "-i", "HEAD~2"}, {"-c", "core.hooksPath=.", "status"}, {"submodule", "foreach", "sh"}} {
		if _, err := proxy.Execute(context.Background(), Request{Tool: "git", CWD: cwd, Args: args}, io.Discard); err == nil {
			t.Fatalf("command %v was not rejected", args)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner called for rejected command: %#v", runner.calls)
	}
}

func testProxy(t *testing.T) (*Proxy, *broker.Mock, *recordingRunner, string) {
	t.Helper()
	hostRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostRoot, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostRoot, "sub", "message.txt"), []byte("message"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec(hostRoot)
	mapper, err := NewMapper("/home/dev", []broker.SessionSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	mock := &broker.Mock{}
	runner := &recordingRunner{broker: mock}
	return NewProxy(mock, mapper, runner), mock, runner, "/home/dev/workspaces/project-123"
}

func testSpec(hostRoot string) broker.SessionSpec {
	hostRoot, _ = filepath.EvalSymlinks(hostRoot)
	return broker.SessionSpec{
		Profile: "work", ProjectID: "1234567890abcdef", Name: "session",
		HostRoot: hostRoot, GuestRoot: "~/workspaces/project-123",
	}
}

func assertOperations(t *testing.T, mock *broker.Mock, want ...broker.Operation) {
	t.Helper()
	if len(mock.Calls) != len(want) {
		t.Fatalf("broker calls = %#v, want %v", mock.Calls, want)
	}
	for i, operation := range want {
		if mock.Calls[i].Operation != operation {
			t.Fatalf("broker call %d = %s, want %s", i, mock.Calls[i].Operation, operation)
		}
	}
}
