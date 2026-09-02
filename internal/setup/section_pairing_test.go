// Proprietary and confidential. All rights reserved.

package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloister.io/internal/vm"
)

func pairingContext(t *testing.T, backend *vm.MockBackend) *SetupContext {
	t.Helper()
	dir := t.TempDir()
	return &SetupContext{
		Profile:      "work",
		Backend:      backend,
		State:        &SetupState{},
		Progress:     &Progress{},
		StatePath:    filepath.Join(dir, "work.yaml"),
		ProgressPath: filepath.Join(dir, "work.progress"),
		LogPath:      filepath.Join(dir, "work.log"),
	}
}

// TestApproveDevicesPreservesTheExitStatus pins the mechanism that made the
// failure unreachable.
//
// The command ended in "|| true", so the shell always exited zero and the
// error check below it could never fire. Every failure became a success, and
// the raw output of the failure was printed immediately above a tick.
func TestApproveDevicesPreservesTheExitStatus(t *testing.T) {
	backend := &vm.MockBackend{SSHCommandOut: "openclaw: gateway not reachable\n"}
	ctx := pairingContext(t, backend)

	if _, err := approvePendingDevices(ctx); err != nil {
		t.Fatalf("approvePendingDevices() error = %v", err)
	}
	if len(backend.SSHCommandCalls) != 1 {
		t.Fatalf("expected one guest command, got %d", len(backend.SSHCommandCalls))
	}
	if strings.Contains(backend.SSHCommandCalls[0].Command, "|| true") {
		t.Errorf("approval command discards its exit status: %s", backend.SSHCommandCalls[0].Command)
	}
}

// TestApproveDevicesDoesNotClaimSuccessAfterFailure is the behaviour the user
// sees. A failed approval must not be recorded as done, because the recorded
// flag short-circuits the whole pairing section on the next run.
func TestApproveDevicesDoesNotClaimSuccessAfterFailure(t *testing.T) {
	backend := &vm.MockBackend{
		SSHCommandOut: "Error: gateway not reachable at 127.0.0.1:8080\n",
		SSHCommandErr: errors.New("ssh command in work: exit status 1"),
	}
	ctx := pairingContext(t, backend)

	err := approveDevices(ctx)
	if err == nil {
		t.Fatal("approveDevices() reported success for a failing command")
	}
	if ctx.State.Pairing.DevicesApproved {
		t.Error("a failed approval was recorded as complete")
	}
	if !strings.Contains(err.Error(), "gateway not reachable") {
		t.Errorf("error %q does not carry the gateway's diagnosis", err)
	}
}

// TestApproveDevicesRecordsSuccessWhenItSucceeds guards the other direction,
// so the fix cannot be "always fail".
func TestApproveDevicesRecordsSuccessWhenItSucceeds(t *testing.T) {
	backend := &vm.MockBackend{SSHCommandOut: "Approved device pixel-9\n"}
	ctx := pairingContext(t, backend)

	if err := approveDevices(ctx); err != nil {
		t.Fatalf("approveDevices() error = %v", err)
	}
	if !ctx.State.Pairing.DevicesApproved {
		t.Error("a successful approval was not recorded")
	}
}

// TestDeviceApprovalSummaryIsOneLine keeps the raw guest output off the
// console. The whole output used to be printed verbatim, which is how a
// failure came to be displayed directly above a success marker.
func TestDeviceApprovalSummaryIsOneLine(t *testing.T) {
	summary := deviceApprovalSummary("connecting to gateway...\nreading pending queue\nApproved device pixel-9\n")

	if strings.Contains(summary, "\n") {
		t.Errorf("summary spans multiple lines: %q", summary)
	}
	if !strings.Contains(summary, "pixel-9") {
		t.Errorf("summary %q does not name the device that was approved", summary)
	}
}

// TestDeviceApprovalSummaryReportsNothingPending covers the case where the
// command succeeds with nothing to do, which is not an approval.
func TestDeviceApprovalSummaryReportsNothingPending(t *testing.T) {
	for _, output := range []string{"", "   \n\n", "No pending devices\n"} {
		summary := deviceApprovalSummary(output)
		if summary != "No pending device" {
			t.Errorf("summary for %q = %q, want %q", output, summary, "No pending device")
		}
	}
}

// TestApprovePendingDevicesWritesDetailToTheLog pins where the detail goes:
// the console gets one line, and the full output stays available for
// diagnosis without being printed.
func TestApprovePendingDevicesWritesDetailToTheLog(t *testing.T) {
	backend := &vm.MockBackend{SSHCommandOut: "connecting to gateway...\nApproved device pixel-9\n"}
	ctx := pairingContext(t, backend)

	summary, err := approvePendingDevices(ctx)
	if err != nil {
		t.Fatalf("approvePendingDevices() error = %v", err)
	}
	if strings.Contains(summary, "connecting to gateway") {
		t.Errorf("summary %q carries detail that belongs in the log", summary)
	}

	logged, err := os.ReadFile(ctx.LogPath)
	if err != nil {
		t.Fatalf("reading the setup log: %v", err)
	}
	if !strings.Contains(string(logged), "connecting to gateway") {
		t.Errorf("the setup log did not record the command output:\n%s", logged)
	}
}
