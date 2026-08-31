package vcsbroker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"cloister.io/internal/broker"
)

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
