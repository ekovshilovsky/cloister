package native_test

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/agent"
	"github.com/ekovshilovsky/cloister/internal/agent/native"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// Compile-time verification that Runtime satisfies the agent.Runtime interface.
// Any future breaking change to the interface signature will be caught at build
// time rather than at runtime.
var _ agent.Runtime = (*native.Runtime)(nil)

// TestRuntime_Start_AlreadyRunning verifies that Start is idempotent: when the
// OpenClaw daemon is already running (pgrep reports "running"), Start must
// return nil without issuing a second "openclaw gateway start" command.
func TestRuntime_Start_AlreadyRunning(t *testing.T) {
	backend := &vm.MockBackend{
		// pgrep probe returns "running", so IsRunning should short-circuit Start.
		SSHCommandOut: "running",
	}
	r := &native.Runtime{}
	if err := r.Start("test", nil, "", "", backend); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	// Exactly one SSH call is expected: the pgrep liveness probe.
	// A second call would indicate that the start command was incorrectly issued.
	if len(backend.SSHCommandCalls) != 1 {
		t.Errorf("expected 1 SSHCommand call (pgrep probe only), got %d", len(backend.SSHCommandCalls))
	}
	for _, call := range backend.SSHCommandCalls {
		if call.Command == "openclaw gateway start --daemon" {
			t.Error("Start must not issue 'openclaw gateway start --daemon' when daemon is already running")
		}
	}
}

// TestRuntime_Start_NotRunning verifies that Start issues the gateway start
// command when the OpenClaw daemon is not yet running.
func TestRuntime_Start_NotRunning(t *testing.T) {
	backend := &vm.MockBackend{
		// pgrep probe returns "stopped", triggering the actual start sequence.
		SSHCommandOut: "stopped",
	}
	r := &native.Runtime{}
	if err := r.Start("test", nil, "", "", backend); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	// Verify the start command was issued after the liveness probe.
	foundStart := false
	for _, call := range backend.SSHCommandCalls {
		if call.Command == "openclaw gateway start --daemon" {
			foundStart = true
			break
		}
	}
	if !foundStart {
		t.Error("Start must issue 'openclaw gateway start --daemon' when daemon is not running")
	}
}

// TestRuntime_Stop verifies that Stop issues the correct gateway shutdown command.
func TestRuntime_Stop(t *testing.T) {
	backend := &vm.MockBackend{}
	r := &native.Runtime{}
	if err := r.Stop("test", backend); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	if len(backend.SSHCommandCalls) != 1 {
		t.Fatalf("expected 1 SSHCommand call, got %d", len(backend.SSHCommandCalls))
	}
	if got := backend.SSHCommandCalls[0].Command; got != "openclaw gateway stop" {
		t.Errorf("expected command %q, got %q", "openclaw gateway stop", got)
	}
}

// TestRuntime_IsRunning_True verifies that IsRunning returns true when the
// pgrep probe reports the daemon is running.
func TestRuntime_IsRunning_True(t *testing.T) {
	backend := &vm.MockBackend{
		SSHCommandOut: "running",
	}
	r := &native.Runtime{}
	if !r.IsRunning("test", backend) {
		t.Error("expected IsRunning to return true when pgrep reports 'running'")
	}
}

// TestRuntime_IsRunning_False verifies that IsRunning returns false when the
// pgrep probe reports the daemon is stopped.
func TestRuntime_IsRunning_False(t *testing.T) {
	backend := &vm.MockBackend{
		SSHCommandOut: "stopped",
	}
	r := &native.Runtime{}
	if r.IsRunning("test", backend) {
		t.Error("expected IsRunning to return false when pgrep reports 'stopped'")
	}
}
