package lume_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
