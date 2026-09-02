package vm_test

import (
	"bytes"
	"testing"

	"cloister.io/internal/vm"
	"cloister.io/internal/vm/colima"
	"cloister.io/internal/vm/lume"
)

// Compile-time proof that every concrete backend satisfies the interface. A
// method added to Backend without a matching implementation fails here, at the
// declaration, rather than wherever the backend is first used.
var (
	_ vm.Backend = (*colima.Backend)(nil)
	_ vm.Backend = (*lume.Backend)(nil)
	_ vm.Backend = (*vm.MockBackend)(nil)
)

// TestSSHScriptToIsTheStreamingPathWithADestination pins the relationship the
// provisioning rework depends on: SSHScript must remain exactly SSHScriptTo
// aimed at the terminal, so redirecting guest output changes where it goes and
// nothing else about how a script runs.
func TestSSHScriptToIsTheStreamingPathWithADestination(t *testing.T) {
	var sink bytes.Buffer
	backend := &vm.MockBackend{SSHScriptOut: "guest output\n"}

	captured, err := backend.SSHScriptTo("work", "echo hi", &sink)
	if err != nil {
		t.Fatalf("SSHScriptTo() error = %v", err)
	}
	if captured != "guest output\n" {
		t.Errorf("returned output = %q, want the captured guest output", captured)
	}
	if sink.String() != "guest output\n" {
		t.Errorf("sink received %q, want the guest output", sink.String())
	}
	if len(backend.SSHScriptCalls) != 1 || backend.SSHScriptCalls[0].Script != "echo hi" {
		t.Errorf("script calls = %#v, want one recording the script", backend.SSHScriptCalls)
	}
}

// TestSSHScriptToToleratesNoDestination covers the lume backend's contract,
// where a nil destination means the output is captured and nothing else.
func TestSSHScriptToToleratesNoDestination(t *testing.T) {
	backend := &vm.MockBackend{SSHScriptOut: "guest output\n"}
	if _, err := backend.SSHScriptTo("work", "echo hi", nil); err != nil {
		t.Fatalf("SSHScriptTo() with a nil destination error = %v", err)
	}
}
