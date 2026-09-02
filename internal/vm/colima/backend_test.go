package colima_test

import (
	"bytes"
	"io"
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

// Stdout and stderr are copied by os/exec concurrently unless they share the
// exact same writer. Exercise both streams hard enough for the race detector
// to catch an unsynchronized shared capture buffer.
func TestSSHScriptToSafelyCapturesConcurrentGuestStreams(t *testing.T) {
	installConcurrentCommand(t, "colima")

	if _, err := (&colima.Backend{}).SSHScriptTo("work", "true", io.Discard); err == nil {
		t.Fatal("SSHScriptTo() error = nil, want the stub command's failure")
	}
}

func TestSSHScriptToPreservesGuestStreamOrder(t *testing.T) {
	installOrderedCommand(t, "colima")

	captured, _ := (&colima.Backend{}).SSHScriptTo("work", "true", io.Discard)
	want := "stdout first\nstderr second\nstdout third\n"
	if captured != want {
		t.Errorf("captured = %q, want %q", captured, want)
	}
}

func installOrderedCommand(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	contents := "#!/bin/sh\n" +
		"printf 'stdout first\\n'\n" +
		"printf 'stderr second\\n' >&2\n" +
		"printf 'stdout third\\n'\n" +
		"exit 17\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
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

func installConcurrentCommand(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, name)
	contents := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ \"$i\" -lt 5000 ]; do printf 'stdout-%04d-abcdefghijklmnopqrstuvwxyz0123456789\\n' \"$i\"; i=$((i + 1)); done &\n" +
		"i=0\n" +
		"while [ \"$i\" -lt 5000 ]; do printf 'stderr-%04d-abcdefghijklmnopqrstuvwxyz0123456789\\n' \"$i\"; i=$((i + 1)); done >&2 &\n" +
		"wait\nexit 17\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}
