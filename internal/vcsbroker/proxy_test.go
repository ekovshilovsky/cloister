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
	Env              []string
	BrokerCallsAtRun int
}

type recordingRunner struct {
	broker   *broker.Mock
	calls    []runnerObservation
	exitCode int
	err      error
}

func (r *recordingRunner) Run(_ context.Context, executable string, args []string, dir string, env []string, output io.Writer) (int, error) {
	r.calls = append(r.calls, runnerObservation{
		Executable:       executable,
		Args:             append([]string(nil), args...),
		Dir:              dir,
		Env:              append([]string(nil), env...),
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

func TestProxyRunsAccountScopedGHOutsideProjectsWithoutBarriers(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		args []string
	}{
		{name: "api from workspace root", cwd: "/home/dev/workspaces", args: []string{"api", "user"}},
		{name: "auth status from unmapped directory", cwd: "/home/dev/other", args: []string{"auth", "status"}},
		{name: "repo list", cwd: "/home/dev/workspaces", args: []string{"repo", "list"}},
		{name: "search", cwd: "/home/dev/workspaces", args: []string{"search", "prs", "fix"}},
		{name: "status", cwd: "/home/dev/workspaces", args: []string{"status"}},
	}
	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proxy, mock, runner, _ := testProxy(t)
			exit, err := proxy.Execute(context.Background(), Request{Tool: "gh", CWD: tc.cwd, Args: tc.args}, io.Discard)
			if err != nil || exit != 0 {
				t.Fatalf("Execute() exit=%d error=%v", exit, err)
			}
			if len(mock.Calls) != 0 {
				t.Fatalf("account-scoped command triggered workspace barriers: %#v", mock.Calls)
			}
			if len(runner.calls) != 1 || runner.calls[0].Dir != hostHome || runner.calls[0].BrokerCallsAtRun != 0 {
				t.Fatalf("runner calls = %#v, want one host-home call before any broker operation", runner.calls)
			}
		})
	}
}

func TestValidateGHCommandScopes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want commandScope
	}{
		{name: "auth status", args: []string{"auth", "status"}, want: commandScopeAccount},
		{name: "api account endpoint", args: []string{"api", "--hostname", "github.com", "user"}, want: commandScopeAccount},
		{name: "repo list", args: []string{"repo", "list", "owner"}, want: commandScopeAccount},
		{name: "search", args: []string{"search", "issues", "bug"}, want: commandScopeAccount},
		{name: "status", args: []string{"status"}, want: commandScopeAccount},
		{name: "api repository path", args: []string{"api", "repos/owner/project"}, want: commandScopeProject},
		{name: "api repository placeholder", args: []string{"api", "repos/{owner}/{repo}"}, want: commandScopeProject},
		{name: "issue", args: []string{"issue", "list"}, want: commandScopeProject},
		{name: "pr", args: []string{"pr", "list"}, want: commandScopeProject},
		{name: "release", args: []string{"release", "list"}, want: commandScopeProject},
		{name: "repo view", args: []string{"repo", "view"}, want: commandScopeProject},
		{name: "run", args: []string{"run", "list"}, want: commandScopeProject},
		{name: "workflow", args: []string{"workflow", "list"}, want: commandScopeProject},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateCommand("gh", tc.args, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("validateCommand(gh %v) scope=%v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestProxyClassifiesRepoScopedGHAPIEndpoints(t *testing.T) {
	repoScoped := [][]string{
		{"api", "repos/owner/project"},
		{"api", "/repos/owner/project/issues"},
		{"api", "https://api.github.com/repos/owner/project"},
		{"api", "branches/{branch}"},
		{"api", "orgs/{owner}/settings"},
	}
	for _, args := range repoScoped {
		proxy, mock, runner, _ := testProxy(t)
		exit, err := proxy.Execute(context.Background(), Request{Tool: "gh", CWD: "/home/dev/workspaces", Args: args}, io.Discard)
		if exit != 125 || err == nil {
			t.Fatalf("gh %v exit=%d error=%v, want unmapped refusal", args, exit, err)
		}
		if len(mock.Calls) != 0 || len(runner.calls) != 0 {
			t.Fatalf("repo-scoped API reached broker or runner: broker=%#v runner=%#v", mock.Calls, runner.calls)
		}
	}
}

func TestProxyRefusesRepoScopedGHOutsideProjectWithGuidance(t *testing.T) {
	proxy, mock, runner, _ := testProxy(t)
	exit, err := proxy.Execute(context.Background(), Request{
		Tool: "gh", CWD: "/home/dev/workspaces", Args: []string{"pr", "list"},
	}, io.Discard)
	want := `guest working directory "/home/dev/workspaces" is outside every registered workspace project; use gh -R owner/repo <command> or cd to ~/workspaces/<project>`
	if exit != 125 || err == nil || err.Error() != want {
		t.Fatalf("Execute() exit=%d error=%q, want exit 125 and %q", exit, err, want)
	}
	if len(mock.Calls) != 0 || len(runner.calls) != 0 {
		t.Fatalf("unmapped command reached broker or runner: broker=%#v runner=%#v", mock.Calls, runner.calls)
	}
}

func TestProxyRunsExplicitlyTargetedGHOutsideProject(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  []string
	}{
		{name: "short repo flag", args: []string{"pr", "list", "-R", "owner/repo"}},
		{name: "long repo flag", args: []string{"--repo=owner/repo", "issue", "list"}},
		{name: "repo environment", args: []string{"workflow", "list"}, env: []string{"GH_REPO=owner/repo"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proxy, mock, runner, _ := testProxy(t)
			exit, err := proxy.Execute(context.Background(), Request{
				Tool: "gh", CWD: "/home/dev/workspaces", Args: tc.args, Env: tc.env,
			}, io.Discard)
			if err != nil || exit != 0 {
				t.Fatalf("Execute() exit=%d error=%v", exit, err)
			}
			if len(mock.Calls) != 0 || len(runner.calls) != 1 || runner.calls[0].BrokerCallsAtRun != 0 {
				t.Fatalf("explicitly targeted command should run without project barriers: broker=%#v runner=%#v", mock.Calls, runner.calls)
			}
			if len(tc.env) > 0 && environmentValue(runner.calls[0].Env, "GH_REPO") != "owner/repo" {
				t.Fatalf("GH_REPO was not forwarded: %#v", runner.calls[0].Env)
			}
		})
	}
}

func TestProxyDoesNotTreatEmptyEffectiveGHRepoAsExplicitTarget(t *testing.T) {
	proxy, mock, runner, _ := testProxy(t)
	exit, err := proxy.Execute(context.Background(), Request{
		Tool: "gh", CWD: "/home/dev/workspaces", Args: []string{"pr", "list"},
		Env: []string{"GH_REPO=owner/repo", "GH_REPO="},
	}, io.Discard)
	if exit != 125 || err == nil {
		t.Fatalf("Execute() exit=%d error=%v, want unmapped refusal", exit, err)
	}
	if len(mock.Calls) != 0 || len(runner.calls) != 0 {
		t.Fatalf("empty effective GH_REPO reached broker or runner: broker=%#v runner=%#v", mock.Calls, runner.calls)
	}
}

func TestProxyRefusesBareGitOutsideProjectWithDashCGuidance(t *testing.T) {
	proxy, mock, runner, _ := testProxy(t)
	exit, err := proxy.Execute(context.Background(), Request{
		Tool: "git", CWD: "/home/dev/workspaces", Args: []string{"status"},
	}, io.Discard)
	want := `guest working directory "/home/dev/workspaces" is outside every registered workspace project; use git -C ~/workspaces/<project> <command>`
	if exit != 2 || err == nil || err.Error() != want {
		t.Fatalf("Execute() exit=%d error=%q, want exit 2 and %q", exit, err, want)
	}
	if len(mock.Calls) != 0 || len(runner.calls) != 0 {
		t.Fatalf("unmapped command reached broker or runner: broker=%#v runner=%#v", mock.Calls, runner.calls)
	}
}

func TestProxyMapsGitDashCFromWorkspaceRoot(t *testing.T) {
	proxy, mock, runner, _ := testProxy(t)
	exit, err := proxy.Execute(context.Background(), Request{
		Tool: "git", CWD: "/home/dev/workspaces", Args: []string{"-C", "project-123", "status"},
	}, io.Discard)
	if err != nil || exit != 0 {
		t.Fatalf("Execute() exit=%d error=%v", exit, err)
	}
	assertOperations(t, mock, broker.OperationFlush, broker.OperationStatus)
	if len(runner.calls) != 1 || runner.calls[0].Args[0] != "status" {
		t.Fatalf("runner calls = %#v", runner.calls)
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
	for _, args := range [][]string{
		{"commit"},
		{"rebase", "-i", "HEAD~2"},
		{"-c", "core.hooksPath=.", "status"},
		{"--git-dir=.git", "status"},
		{"--work-tree", ".", "status"},
		{"--work-tree=.", "status"},
		{"submodule", "foreach", "sh"},
	} {
		if _, err := proxy.Execute(context.Background(), Request{Tool: "git", CWD: cwd, Args: args}, io.Discard); err == nil {
			t.Fatalf("command %v was not rejected", args)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner called for rejected command: %#v", runner.calls)
	}
}

func TestProxyRejectsAccountLevelAndHostRedirectingCommands(t *testing.T) {
	rejected := []struct {
		tool string
		args []string
	}{
		{"gh", []string{"api", "-X", "POST", "/user/keys", "-f", "key=abc"}},
		{"gh", []string{"api", "--method", "DELETE", "/repos/o/r"}},
		{"gh", []string{"api", "-f", "query=mutation{...}", "graphql"}},
		{"gh", []string{"api", "user/keys", "--input", "-"}},
		{"git", []string{"--exec-path=/home/dev/workspaces/project-123", "status"}},
		{"git", []string{"--namespace", "evil", "log"}},
	}
	for _, tc := range rejected {
		proxy, _, runner, cwd := testProxy(t)
		if _, err := proxy.Execute(context.Background(), Request{Tool: tc.tool, CWD: cwd, Args: tc.args}, io.Discard); err == nil {
			t.Fatalf("%s %v was not rejected", tc.tool, tc.args)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("runner ran a rejected command: %#v", runner.calls)
		}
	}

	// Read-only gh api must still pass classification and run host-side.
	proxy, _, runner, cwd := testProxy(t)
	if _, err := proxy.Execute(context.Background(), Request{Tool: "gh", CWD: cwd, Args: []string{"api", "/repos/o/r"}}, io.Discard); err != nil {
		t.Fatalf("read-only gh api rejected: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("read-only gh api did not run: %#v", runner.calls)
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

func environmentValue(env []string, name string) string {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}
