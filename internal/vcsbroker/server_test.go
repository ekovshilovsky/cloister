package vcsbroker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloister.io/internal/broker"
)

type blockingPostBarrier struct {
	mu          sync.Mutex
	flushes     int
	postStarted chan struct{}
	releasePost chan struct{}
}

type statelessSyncBroker struct{}

func (statelessSyncBroker) Create(context.Context, broker.SessionSpec) error    { return nil }
func (statelessSyncBroker) Flush(context.Context, broker.SessionSpec) error     { return nil }
func (statelessSyncBroker) Pause(context.Context, broker.SessionSpec) error     { return nil }
func (statelessSyncBroker) Resume(context.Context, broker.SessionSpec) error    { return nil }
func (statelessSyncBroker) Terminate(context.Context, broker.SessionSpec) error { return nil }
func (statelessSyncBroker) Status(_ context.Context, spec broker.SessionSpec) (broker.Status, error) {
	return broker.Status{State: broker.StateActive, HostRoot: spec.HostRoot, GuestRoot: spec.GuestRoot}, nil
}

type concurrencyRunner struct {
	mu      sync.Mutex
	active  int
	maximum int
	started chan string
	release chan struct{}
}

type largeOutputRunner struct {
	done chan struct{}
}

func (r largeOutputRunner) Run(_ context.Context, _ string, _ []string, _ string, _ []string, output io.Writer) (int, error) {
	const outputSize = 8 << 20
	_, err := io.CopyN(output, strings.NewReader(strings.Repeat("x", outputSize)), outputSize)
	close(r.done)
	return 0, err
}

func TestHalfOpenResponseReaderDoesNotPinCommandDrain(t *testing.T) {
	previousTimeout := responseWriteStallTimeout
	responseWriteStallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { responseWriteStallTimeout = previousTimeout })
	root, _ := filepath.EvalSymlinks(t.TempDir())
	mapper, err := NewMapper("/home/guest", []broker.SessionSpec{{
		Profile: "example", ProjectID: "project", Name: "project",
		HostRoot: root, GuestRoot: "~/workspaces/project",
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := largeOutputRunner{done: make(chan struct{})}
	server, err := StartServer(NewProxy(statelessSyncBroker{}, mapper, runner), "reader-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	connection, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", server.Port()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	body := "tool=git&cwd=%2Fhome%2Fguest%2Fworkspaces%2Fproject&arg=status"
	_, err = fmt.Fprintf(connection, "POST /v1/exec HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer reader-token\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.done:
	case <-time.After(time.Second):
		t.Fatal("client that stopped reading blocked command execution")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if commands, err := server.Pause(ctx); err != nil || len(commands) != 0 {
		t.Fatalf("half-open response pinned command drain: commands=%#v error=%v", commands, err)
	}
}

func (r *concurrencyRunner) Run(_ context.Context, _ string, _ []string, dir string, _ []string, output io.Writer) (int, error) {
	r.mu.Lock()
	r.active++
	if r.active > r.maximum {
		r.maximum = r.active
	}
	r.mu.Unlock()
	r.started <- dir
	<-r.release
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	_, _ = io.WriteString(output, "ok\n")
	return 0, nil
}

func TestServerConcurrentCommandsUseOnlyProjectScopedSerialization(t *testing.T) {
	rootA, _ := filepath.EvalSymlinks(t.TempDir())
	rootB, _ := filepath.EvalSymlinks(t.TempDir())
	mapper, err := NewMapper("/home/guest", []broker.SessionSpec{
		{Profile: "example", ProjectID: "a", Name: "a", HostRoot: rootA, GuestRoot: "~/workspaces/a"},
		{Profile: "example", ProjectID: "b", Name: "b", HostRoot: rootB, GuestRoot: "~/workspaces/b"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		guestRoots []string
		wantMax    int
	}{
		{name: "different projects run concurrently", guestRoots: []string{"/home/guest/workspaces/a", "/home/guest/workspaces/b"}, wantMax: 2},
		{name: "same project serializes without rejection", guestRoots: []string{"/home/guest/workspaces/a", "/home/guest/workspaces/a"}, wantMax: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &concurrencyRunner{started: make(chan string, 2), release: make(chan struct{})}
			server, err := StartServer(NewProxy(statelessSyncBroker{}, mapper, runner), "concurrency-token")
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			results := make(chan int, 2)
			for _, cwd := range test.guestRoots {
				go func() {
					form := url.Values{"tool": {"git"}, "cwd": {cwd}, "arg": {"status"}}
					req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/exec", server.Port()), strings.NewReader(form.Encode()))
					req.Header.Set("Authorization", "Bearer concurrency-token")
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					response, requestErr := http.DefaultClient.Do(req)
					if requestErr != nil {
						results <- 0
						return
					}
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
					results <- response.StatusCode
				}()
			}

			<-runner.started
			if test.wantMax == 2 {
				select {
				case <-runner.started:
				case <-time.After(time.Second):
					t.Fatal("second project did not run concurrently")
				}
			} else {
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					status := server.Status()
					if len(status.Commands) == 2 {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if len(server.Status().Commands) != 2 {
					t.Fatal("second same-project command was not admitted")
				}
			}
			close(runner.release)
			for range 2 {
				if status := <-results; status != http.StatusOK {
					t.Fatalf("concurrent response status = %d", status)
				}
			}
			runner.mu.Lock()
			maximum := runner.maximum
			runner.mu.Unlock()
			if maximum != test.wantMax {
				t.Fatalf("maximum concurrent runners = %d, want %d", maximum, test.wantMax)
			}
		})
	}
}

func (b *blockingPostBarrier) Create(context.Context, broker.SessionSpec) error { return nil }
func (b *blockingPostBarrier) Pause(context.Context, broker.SessionSpec) error  { return nil }
func (b *blockingPostBarrier) Resume(context.Context, broker.SessionSpec) error { return nil }
func (b *blockingPostBarrier) Terminate(context.Context, broker.SessionSpec) error {
	return nil
}
func (b *blockingPostBarrier) Flush(ctx context.Context, _ broker.SessionSpec) error {
	b.mu.Lock()
	b.flushes++
	flush := b.flushes
	b.mu.Unlock()
	if flush != 2 {
		return nil
	}
	close(b.postStarted)
	select {
	case <-b.releasePost:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *blockingPostBarrier) Status(_ context.Context, spec broker.SessionSpec) (broker.Status, error) {
	return broker.Status{State: broker.StateActive, HostRoot: spec.HostRoot, GuestRoot: spec.GuestRoot}, nil
}

func TestServerStreamsOutputAndReturnsExactHostExitCode(t *testing.T) {
	proxy, mock, runner, cwd := testProxy(t)
	runner.exitCode = 37
	server, err := StartServer(proxy, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	form := url.Values{"tool": {"git"}, "cwd": {cwd}, "arg": {"status"}}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/exec", server.Port()), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer secret-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK || string(body) != "host output\n" {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
	if got := response.Trailer.Get(exitTrailer); got != strconv.Itoa(37) {
		t.Fatalf("exit trailer = %q, want 37", got)
	}
	assertOperations(t, mock, broker.OperationFlush, broker.OperationStatus)
}

func TestServerRejectsUnauthenticatedRequestsBeforeExecution(t *testing.T) {
	proxy, mock, runner, cwd := testProxy(t)
	server, err := StartServer(proxy, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	form := url.Values{"tool": {"git"}, "cwd": {cwd}, "arg": {"status"}}
	response, err := http.PostForm(fmt.Sprintf("http://127.0.0.1:%d/v1/exec", server.Port()), form)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	if len(mock.Calls) != 0 || len(runner.calls) != 0 {
		t.Fatalf("unauthenticated request executed: broker=%#v runner=%#v", mock.Calls, runner.calls)
	}
}

func TestStartServerRequiresToken(t *testing.T) {
	if _, err := StartServer(nil, ""); err == nil {
		t.Fatal("StartServer accepted an empty token")
	}
}

func TestServerHealthRequiresCurrentToken(t *testing.T) {
	proxy, _, _, _ := testProxy(t)
	server, err := StartServer(proxy, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/health", server.Port())
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer secret-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("authenticated health status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	response, err = http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated health status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

func TestServerDrainCompletesRealGitCommandAndRejectsNewWork(t *testing.T) {
	repo := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "change.txt")

	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	spec := broker.SessionSpec{Profile: "example", ProjectID: "project", Name: "project", HostRoot: repo, GuestRoot: "~/workspaces/project"}
	guestRoot := "/home/guest/workspaces/project"
	mapper, err := NewMapper("/home/guest", []broker.SessionSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	barrier := &blockingPostBarrier{postStarted: make(chan struct{}), releasePost: make(chan struct{})}
	server, err := StartServer(NewProxy(barrier, mapper, nil), "drain-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	request := func(args ...string) (*http.Response, error) {
		form := url.Values{"tool": {"git"}, "cwd": {guestRoot}}
		for _, arg := range args {
			form.Add("arg", arg)
		}
		req, reqErr := http.NewRequestWithContext(context.Background(), http.MethodPost,
			fmt.Sprintf("http://127.0.0.1:%d/v1/exec", server.Port()), strings.NewReader(form.Encode()))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Authorization", "Bearer drain-token")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return http.DefaultClient.Do(req)
	}

	type completedResponse struct {
		status  int
		trailer string
		body    []byte
		err     error
	}
	commandResult := make(chan completedResponse, 1)
	go func() {
		response, requestErr := request("commit", "-m", "complete before shutdown")
		if requestErr != nil {
			commandResult <- completedResponse{err: requestErr}
			return
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		commandResult <- completedResponse{status: response.StatusCode, trailer: response.Trailer.Get(exitTrailer), body: body, err: readErr}
	}()
	select {
	case <-barrier.postStarted:
	case result := <-commandResult:
		t.Fatalf("real git request completed before post-command barrier: status=%d trailer=%q body=%q err=%v", result.status, result.trailer, result.body, result.err)
	case <-time.After(5 * time.Second):
		barrier.mu.Lock()
		flushes := barrier.flushes
		barrier.mu.Unlock()
		t.Fatalf("real git command did not reach its post-command barrier (flushes=%d)", flushes)
	}
	drainDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, drainErr := server.Drain(ctx)
		drainDone <- drainErr
	}()
	deadline := time.Now().Add(time.Second)
	for !server.IsDraining() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	retryResponse, err := request("status")
	if err != nil {
		t.Fatal(err)
	}
	retryBody, err := io.ReadAll(retryResponse.Body)
	retryResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if retryResponse.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(retryBody), "restarting; retry") {
		t.Fatalf("request during drain status=%d body=%q", retryResponse.StatusCode, retryBody)
	}

	close(barrier.releasePost)
	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not finish draining")
	}
	select {
	case result := <-commandResult:
		if result.err != nil {
			t.Fatalf("in-flight client lost its response: %v", result.err)
		}
		if result.status != http.StatusOK || result.trailer != "0" {
			t.Fatalf("in-flight response status=%d trailer=%q body=%q", result.status, result.trailer, result.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight client did not receive its result")
	}
	if subject := runGit("log", "-1", "--format=%s"); subject != "complete before shutdown" {
		t.Fatalf("host command result = %q", subject)
	}
}

func TestProbeHostRetriesTransientFailureAndRejectsRepeatedFailure(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	alwaysFail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if r.Header.Get("Authorization") != "Bearer probe-token" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if alwaysFail || attempts == 1 {
			http.Error(w, "transient", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	if ProbeHost(port, "probe-token") != HostProbeHealthy {
		t.Fatal("one transient host probe failure replaced a healthy daemon")
	}
	mu.Lock()
	gotAttempts := attempts
	mu.Unlock()
	if gotAttempts != 2 {
		t.Fatalf("transient probe attempts = %d, want 2", gotAttempts)
	}

	mu.Lock()
	alwaysFail = true
	mu.Unlock()
	if ProbeHost(port, "probe-token") != HostProbeDead {
		t.Fatal("repeated host probe failures were accepted")
	}
}

func TestProbeHostDistinguishesIntentionalDrain(t *testing.T) {
	proxy, _, _, _ := testProxy(t)
	server, err := StartServer(proxy, "drain-probe-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := server.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if status := ProbeHost(server.Port(), "drain-probe-token"); status != HostProbeDraining {
		t.Fatalf("draining host probe = %v", status)
	}
}

func TestServerDrainTimeoutReportsActiveCommandAndProject(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := NewMapper("/home/guest", []broker.SessionSpec{{
		Profile: "example", ProjectID: "project", Name: "project",
		HostRoot: repo, GuestRoot: "~/workspaces/project",
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	server, err := StartServer(NewProxy(&broker.Mock{}, mapper, runner), "timeout-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(runner.release)
		_ = server.Close()
	})

	form := url.Values{
		"tool": {"git"}, "cwd": {"/home/guest/workspaces/project"},
		"arg": {"push", "origin", "main"},
	}
	request, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/exec", server.Port()), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer timeout-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	commands, err := server.Drain(ctx)
	cancel()
	if err == nil || len(commands) != 1 {
		t.Fatalf("Drain() commands=%#v error=%v", commands, err)
	}
	command := commands[0]
	if command.Tool != "git" || strings.Join(command.Args, " ") != "push origin main" || command.Project != "/home/guest/workspaces/project" {
		t.Fatalf("active command report = %#v", command)
	}
}

type blockingRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingRunner) Run(ctx context.Context, _ string, _ []string, _ string, _ []string, _ io.Writer) (int, error) {
	close(r.started)
	select {
	case <-r.release:
		return 0, nil
	case <-ctx.Done():
		return 125, ctx.Err()
	}
}
