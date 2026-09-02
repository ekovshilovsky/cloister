package lume_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloister.io/internal/vm"
	"cloister.io/internal/vm/lume"
)

// Compile-time assertions that *Backend satisfies vm.Backend, vm.NATNetworker,
// and vm.GoldenImageManager. If any required method is missing or has an
// incorrect signature, these assignments will fail at compile time with a clear
// diagnostic.
var _ vm.Backend = (*lume.Backend)(nil)
var _ vm.NATNetworker = (*lume.Backend)(nil)
var _ vm.GoldenImageManager = (*lume.Backend)(nil)

func TestSSHScriptToKeepsDirectedOutputOutOfTheError(t *testing.T) {
	installFailingCommand(t, "ssh", "guest diagnostic\n")
	var sink bytes.Buffer

	captured, err := (&lume.Backend{}).SSHScriptTo("work", "false", &sink)
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
	installFailingCommand(t, "ssh", "only diagnostic\n")

	_, err := (&lume.Backend{}).SSHScriptTo("work", "false", nil)
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
	if name == "ssh" {
		lume := filepath.Join(dir, "lume")
		contents := "#!/bin/sh\nprintf '%s' '[{\"ipAddress\":\"127.0.0.1\"}]'\n"
		if err := os.WriteFile(lume, []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// TestVMName verifies that VMName correctly prepends the cloister prefix to a
// profile name, producing the Lume VM name used internally.
func TestVMName(t *testing.T) {
	cases := []struct {
		profile string
		want    string
	}{
		{"dev", "cloister-dev"},
		{"work", "cloister-work"},
		{"personal", "cloister-personal"},
		{"", "cloister-"},
	}

	for _, tc := range cases {
		got := lume.VMName(tc.profile)
		if got != tc.want {
			t.Errorf("VMName(%q) = %q, want %q", tc.profile, got, tc.want)
		}
	}
}

// TestProfileFromVMName verifies that ProfileFromVMName correctly strips the
// cloister prefix from a VM name, and returns an empty string for VM names
// that were not created by cloister.
func TestProfileFromVMName(t *testing.T) {
	cases := []struct {
		vmName string
		want   string
	}{
		{"cloister-dev", "dev"},
		{"cloister-work", "work"},
		{"cloister-personal", "personal"},
		// VM names that do not carry the cloister prefix must return empty string.
		{"other-vm", ""},
		{"lume-default", ""},
		{"default", ""},
		{"", ""},
		// A string equal to the bare prefix with no profile segment following it.
		{"cloister-", ""},
	}

	for _, tc := range cases {
		got := lume.ProfileFromVMName(tc.vmName)
		if got != tc.want {
			t.Errorf("ProfileFromVMName(%q) = %q, want %q", tc.vmName, got, tc.want)
		}
	}
}

// --verbose promises the guest output as it happens. A backend that hands the
// output over only once the command has exited cannot deliver that, however the
// flag is documented, and a long provisioning step looks like a hang.
func TestSSHScriptToStreamsOutputWhileTheCommandRuns(t *testing.T) {
	const linger = 500 * time.Millisecond
	installLingeringCommand(t, "ssh", "first line\n", linger)

	sink := &stampingWriter{}
	if _, err := (&lume.Backend{}).SSHScriptTo("work", "true", sink); err != nil {
		t.Fatalf("SSHScriptTo() error = %v", err)
	}
	finished := time.Now()

	if !sink.wrote {
		t.Fatal("nothing reached the sink")
	}
	// The measurement is the gap between the first output and the command
	// exiting, not the time to first output: process startup is slow and
	// variable under a loaded test run, and it cancels out of a difference
	// between two points inside the run. Streaming leaves roughly the whole
	// linger in this gap; buffering leaves none of it.
	if lead := finished.Sub(sink.first); lead < linger/2 {
		t.Errorf("first output reached the sink only %v before the command exited, out of the %v it lingered; it was held until the command finished",
			lead.Round(time.Millisecond), linger)
	}
	if !strings.Contains(sink.buf.String(), "first line") {
		t.Errorf("sink = %q, want the guest output", sink.buf.String())
	}
}

// stampingWriter records when its first output arrived, which against the time
// the command exited is the difference between streaming and buffering.
type stampingWriter struct {
	first time.Time
	wrote bool
	buf   bytes.Buffer
}

func (w *stampingWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.first, w.wrote = time.Now(), true
	}
	return w.buf.Write(p)
}

// installLingeringCommand puts a stand-in on PATH that prints, keeps running,
// and then exits successfully.
func installLingeringCommand(t *testing.T, name, output string, linger time.Duration) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, name)
	// The test replaces PATH with this directory alone, so the stub restores
	// enough of one to reach sleep.
	contents := fmt.Sprintf("#!/bin/sh\nPATH=/usr/bin:/bin\nprintf '%%s' '%s'\nsleep %.2f\n", output, linger.Seconds())
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	if name == "ssh" {
		lumeStub := filepath.Join(dir, "lume")
		stub := "#!/bin/sh\nprintf '%s' '[{\"ipAddress\":\"127.0.0.1\"}]'\n"
		if err := os.WriteFile(lumeStub, []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// Streaming replaced CombinedOutput, which folded stderr in. Both streams still
// have to reach the sink, and through one destination so they interleave in the
// order the guest produced them rather than arriving as two separate runs.
func TestSSHScriptToCapturesBothGuestStreamsInOrder(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\nPATH=/usr/bin:/bin\n" +
		"printf 'to stdout\\n'\n" +
		"printf 'to stderr\\n' >&2\n" +
		"printf 'stdout again\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	lumeStub := "#!/bin/sh\nprintf '%s' '[{\"ipAddress\":\"127.0.0.1\"}]'\n"
	if err := os.WriteFile(filepath.Join(dir, "lume"), []byte(lumeStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	var sink bytes.Buffer
	captured, err := (&lume.Backend{}).SSHScriptTo("work", "true", &sink)
	if err != nil {
		t.Fatalf("SSHScriptTo() error = %v", err)
	}

	want := "to stdout\nto stderr\nstdout again\n"
	if captured != want {
		t.Errorf("captured = %q, want %q", captured, want)
	}
	if sink.String() != want {
		t.Errorf("sink = %q, want %q", sink.String(), want)
	}
}

// Verbose list and lifecycle commands capture stdout and stderr for error
// reporting. The race detector verifies that their shared capture remains safe
// when both process streams are busy at once.
func TestListSafelyCapturesConcurrentStreams(t *testing.T) {
	installConcurrentLumeCommand(t)

	discardProcessOutput(t, func() {
		if _, err := (&lume.Backend{}).List(true); err == nil {
			t.Fatal("List() error = nil, want the stub command's failure")
		}
	})
}

func TestRunLumeSafelyCapturesConcurrentStreams(t *testing.T) {
	installConcurrentLumeCommand(t)

	discardProcessOutput(t, func() {
		if err := (&lume.Backend{}).Stop("work", true); err == nil {
			t.Fatal("Stop() error = nil, want the stub command's failure")
		}
	})
}

func TestVerboseLifecycleOutputGoesToStderr(t *testing.T) {
	dir := t.TempDir()
	contents := "#!/bin/sh\n" +
		"printf 'lifecycle stdout\\n'\n" +
		"printf 'lifecycle stderr\\n' >&2\n"
	if err := os.WriteFile(filepath.Join(dir, "lume"), []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := (&lume.Backend{}).Stop("work", true); err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
		})
	})

	if stdout != "" {
		t.Errorf("verbose Stop() stdout = %q, want empty", stdout)
	}
	for _, want := range []string{"lifecycle stdout", "lifecycle stderr"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("verbose Stop() stderr = %q, want %q", stderr, want)
		}
	}
}

func TestListParsesStdoutWhenLumeWarnsOnStderr(t *testing.T) {
	dir := t.TempDir()
	contents := "#!/bin/sh\n" +
		"printf '%s' '[{\"name\":\"cloister-work\",\"status\":\"running\"}]'\n" +
		"printf 'warning: stale cache\\n' >&2\n"
	if err := os.WriteFile(filepath.Join(dir, "lume"), []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	vms, err := (&lume.Backend{}).List(false)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(vms) != 1 || vms[0].Name != "cloister-work" {
		t.Fatalf("List() = %#v, want the VM from stdout", vms)
	}
}

func TestListParseErrorIncludesStderrDiagnostic(t *testing.T) {
	dir := t.TempDir()
	contents := "#!/bin/sh\n" +
		"printf 'not-json'\n" +
		"printf 'warning: stale cache\\n' >&2\n"
	if err := os.WriteFile(filepath.Join(dir, "lume"), []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	_, err := (&lume.Backend{}).List(false)
	if err == nil {
		t.Fatal("List() error = nil, want JSON parse failure")
	}
	if !strings.Contains(err.Error(), "warning: stale cache") {
		t.Errorf("List() error = %q, want retained stderr diagnostic", err)
	}
}

func TestVerboseCommandsPreserveDiagnostics(t *testing.T) {
	installOrderedLumeCommand(t)

	discardProcessOutput(t, func() {
		_, listErr := (&lume.Backend{}).List(true)
		assertOutputContains(t, listErr, "stdout first", "stdout third", "stderr second")

		stopErr := (&lume.Backend{}).Stop("work", true)
		assertOutputOrder(t, stopErr)
	})
}

func TestSSHCommandDoesNotPrintTargetDiagnosticsByDefault(t *testing.T) {
	installSuccessfulSSHCommands(t)

	got := captureStderr(t, func() {
		if _, err := (&lume.Backend{}).SSHCommand("work", "true"); err != nil {
			t.Fatalf("SSHCommand() error = %v", err)
		}
	})

	if strings.Contains(got, "SSH target for") {
		t.Errorf("default SSHCommand() leaked target diagnostics: %q", got)
	}
}

func TestSSHCommandPrintsTargetDiagnosticsWhenVerbose(t *testing.T) {
	installSuccessfulSSHCommands(t)
	backend := &lume.Backend{}
	backend.SetVerbose(true)

	got := captureStderr(t, func() {
		if _, err := backend.SSHCommand("work", "true"); err != nil {
			t.Fatalf("SSHCommand() error = %v", err)
		}
	})

	if !strings.Contains(got, "SSH target for work: using IP 127.0.0.1") {
		t.Errorf("verbose SSHCommand() omitted target diagnostic: %q", got)
	}
}

func installConcurrentLumeCommand(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	contents := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ \"$i\" -lt 5000 ]; do printf 'stdout-%04d-abcdefghijklmnopqrstuvwxyz0123456789\\n' \"$i\"; i=$((i + 1)); done &\n" +
		"i=0\n" +
		"while [ \"$i\" -lt 5000 ]; do printf 'stderr-%04d-abcdefghijklmnopqrstuvwxyz0123456789\\n' \"$i\"; i=$((i + 1)); done >&2 &\n" +
		"wait\nexit 17\n"
	if err := os.WriteFile(filepath.Join(dir, "lume"), []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func installOrderedLumeCommand(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	contents := "#!/bin/sh\n" +
		"printf 'stdout first\\n'\n" +
		"printf 'stderr second\\n' >&2\n" +
		"printf 'stdout third\\n'\n" +
		"exit 17\n"
	if err := os.WriteFile(filepath.Join(dir, "lume"), []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func installSuccessfulSSHCommands(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	lumeStub := "#!/bin/sh\nprintf '%s' '[{\"ipAddress\":\"127.0.0.1\"}]'\n"
	if err := os.WriteFile(filepath.Join(dir, "lume"), []byte(lumeStub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func discardProcessOutput(t *testing.T, fn func()) {
	t.Helper()
	discard, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer discard.Close()

	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = discard, discard
	defer func() { os.Stdout, os.Stderr = stdout, stderr }()
	fn()
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = stderr }()

	drained := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, read)
		drained <- buf.String()
	}()
	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	got := <-drained
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return got
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	defer func() { os.Stdout = stdout }()

	drained := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, read)
		drained <- buf.String()
	}()
	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	got := <-drained
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return got
}

func assertOutputOrder(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("command error = nil, want the stub command's failure")
	}
	message := err.Error()
	first := strings.Index(message, "stdout first")
	second := strings.Index(message, "stderr second")
	third := strings.Index(message, "stdout third")
	if first < 0 || second < first || third < second {
		t.Errorf("combined output is out of order: %q", message)
	}
}

func assertOutputContains(t *testing.T, err error, diagnostics ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("command error = nil, want the stub command's failure")
	}
	for _, diagnostic := range diagnostics {
		if !strings.Contains(err.Error(), diagnostic) {
			t.Errorf("command error omitted %q: %q", diagnostic, err)
		}
	}
}
