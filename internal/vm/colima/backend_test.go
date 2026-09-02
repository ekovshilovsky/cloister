package colima_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloister.io/internal/vm"
	"cloister.io/internal/vm/colima"
)

// Compile-time assertion that *Backend satisfies the vm.Backend interface.
// If any required method is missing or has an incorrect signature, this
// assignment will fail at compile time with a clear diagnostic.
var _ vm.Backend = (*colima.Backend)(nil)

func TestSSHScriptToKeepsDirectedOutputOutOfTheError(t *testing.T) {
	installFailingCommand(t, "colima", "guest diagnostic\n")
	var sink bytes.Buffer

	captured, err := (&colima.Backend{}).SSHScriptTo("work", "false", &sink)
	if err == nil {
		t.Fatal("SSHScriptTo() error = nil")
	}
	if captured != "guest diagnostic\n" || sink.String() != captured {
		t.Fatalf("captured = %q, sink = %q", captured, sink.String())
	}
	if strings.Contains(err.Error(), "guest diagnostic") {
		t.Errorf("directed guest output was duplicated in the error: %q", err)
	}
}

func TestSSHScriptToKeepsCapturedOutputInSinklessError(t *testing.T) {
	installFailingCommand(t, "colima", "only diagnostic\n")

	_, err := (&colima.Backend{}).SSHScriptTo("work", "false", nil)
	if err == nil || !strings.Contains(err.Error(), "only diagnostic") {
		t.Errorf("sinkless SSHScriptTo() error = %q, want captured output", err)
	}
}

func installFailingCommand(t *testing.T, name, output string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, name)
	contents := "#!/bin/sh\nprintf '%s' '" + output + "'\nexit 17\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}
