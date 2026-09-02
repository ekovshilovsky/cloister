// Proprietary and confidential. All rights reserved.

package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"cloister.io/internal/vm"
)

// TestColimaLogsSurfacesTheContainerError pins that the message the user needs
// survives.
//
// The backend returns the guest's combined output alongside the error, and the
// output is where the diagnosis lives: "No such container" names a fixable
// situation, while the wrapped transport error only says a command exited
// non-zero. Discarding the output left the user with the latter.
func TestColimaLogsSurfacesTheContainerError(t *testing.T) {
	backend := &vm.MockBackend{
		SSHCommandOut: "Error response from daemon: No such container: work-gateway\n",
		SSHCommandErr: errors.New("colima ssh command in cloister-work: exit status 1"),
	}

	err := colimaLogs("work", backend, false)
	if err == nil {
		t.Fatal("colimaLogs() returned no error for a failing backend")
	}
	if !strings.Contains(err.Error(), "No such container") {
		t.Errorf("error %q does not carry the guest's diagnosis", err)
	}
}

// TestLumeLogsSurfacesTheGuestError covers the other backend, which reads a
// gateway log file rather than a container.
func TestLumeLogsSurfacesTheGuestError(t *testing.T) {
	backend := &vm.MockBackend{
		SSHCommandOut: "tail: cannot open '/home/lume/.openclaw/logs/gateway.log': Permission denied\n",
		SSHCommandErr: errors.New("ssh command in work: exit status 1"),
	}

	err := lumeLogs("work", backend, false)
	if err == nil {
		t.Fatal("lumeLogs() returned no error for a failing backend")
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("error %q does not carry the guest's diagnosis", err)
	}
}

// TestLogsErrorIsBoundedToOneLine keeps the fix from recreating the problem it
// solves. The command asks for a hundred lines of log, so a failure can arrive
// behind a large amount of ordinary output; embedding all of it would put a
// wall of text in an error message.
func TestLogsErrorIsBoundedToOneLine(t *testing.T) {
	var output strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&output, "2026-09-01T00:00:%02dZ routine gateway log line %d\n", i%60, i)
	}
	output.WriteString("Error response from daemon: No such container: work-gateway\n")

	backend := &vm.MockBackend{
		SSHCommandOut: output.String(),
		SSHCommandErr: errors.New("colima ssh command in cloister-work: exit status 1"),
	}

	err := colimaLogs("work", backend, false)
	if err == nil {
		t.Fatal("colimaLogs() returned no error for a failing backend")
	}
	message := err.Error()
	if strings.Contains(message, "\n") {
		t.Errorf("error spans multiple lines:\n%s", message)
	}
	if len(message) > 300 {
		t.Errorf("error is %d bytes, want a bounded single line: %s", len(message), message)
	}
	if !strings.Contains(message, "No such container") {
		t.Errorf("error %q dropped the diagnosis while bounding the output", message)
	}
	if strings.Contains(message, "routine gateway log line") {
		t.Errorf("error embeds ordinary log output: %s", message)
	}
}

// TestLogsErrorFallsBackToTheTransportError covers a backend that fails
// without producing any guest output, where the wrapped error is all there is.
func TestLogsErrorFallsBackToTheTransportError(t *testing.T) {
	backend := &vm.MockBackend{
		SSHCommandOut: "   \n\n",
		SSHCommandErr: errors.New("colima is not running"),
	}

	err := colimaLogs("work", backend, false)
	if err == nil {
		t.Fatal("colimaLogs() returned no error for a failing backend")
	}
	if !strings.Contains(err.Error(), "colima is not running") {
		t.Errorf("error %q lost the only diagnosis available", err)
	}
}

// TestLogsErrorTruncatesAnOverlongLine covers guest output that carries no
// newline at all. Bounding by line count alone leaves the error unbounded when
// the guest writes one very long line, which a stack trace or a JSON error
// body from a daemon does.
func TestLogsErrorTruncatesAnOverlongLine(t *testing.T) {
	backend := &vm.MockBackend{
		SSHCommandOut: "Error response from daemon: " + strings.Repeat("detail ", 800),
		SSHCommandErr: errors.New("colima ssh command in cloister-work: exit status 1"),
	}

	err := colimaLogs("work", backend, false)
	if err == nil {
		t.Fatal("colimaLogs() returned no error for a failing backend")
	}
	if len(err.Error()) > 300 {
		t.Errorf("error is %d bytes, want it bounded", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "Error response from daemon") {
		t.Errorf("truncation dropped the start of the diagnosis: %s", err)
	}
}
