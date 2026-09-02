package lume_test

import (
	"bytes"
	"fmt"
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

	sink := &stampingWriter{start: time.Now()}
	if _, err := (&lume.Backend{}).SSHScriptTo("work", "true", sink); err != nil {
		t.Fatalf("SSHScriptTo() error = %v", err)
	}

	if !sink.wrote {
		t.Fatal("nothing reached the sink")
	}
	if sink.first > linger/2 {
		t.Errorf("first output reached the sink after %v, with the command still running for %v; it was held until the command exited",
			sink.first.Round(time.Millisecond), linger)
	}
	if !strings.Contains(sink.buf.String(), "first line") {
		t.Errorf("sink = %q, want the guest output", sink.buf.String())
	}
}

// stampingWriter records how long after the command started its first output
// arrived, which is the difference between streaming and buffering.
type stampingWriter struct {
	start time.Time
	first time.Duration
	wrote bool
	buf   bytes.Buffer
}

func (w *stampingWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.first, w.wrote = time.Since(w.start), true
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
