// Proprietary and confidential. All rights reserved.

package lifecycle

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"cloister.io/internal/broker"
	"cloister.io/internal/vm"
)

// TestPreflightDoesNotRepeatProfileWideFactsPerProject pins the volume of
// the per-project preflight.
//
// Activating a workspace collection runs this once per project. A profile here
// carries 78 of them, so anything printed unconditionally is printed 78 times
// on every entry. The mode's semantics are one fact about the profile, not one
// per project, and the command layer already states it once per profile behind
// a suppression file; repeating it here buried the warnings that do differ per
// project in 78 lines that do not.
func TestPreflightDoesNotRepeatProfileWideFactsPerProject(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := broker.BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	coordinator := NewCoordinator(&vm.MockBackend{})
	coordinator.Stderr = &stderr

	if err := coordinator.preflightBroker(&spec); err != nil {
		t.Fatalf("preflightBroker() error = %v", err)
	}

	// Not "stderr is empty": macOS stamps com.apple.provenance on ordinary
	// files, so the per-project metadata warning legitimately fires almost
	// everywhere. The invariant is narrower -- the profile-wide fact must not
	// be one of the lines repeated per project.
	if strings.Contains(stderr.String(), "local-filesystem equivalence") {
		t.Errorf("preflight repeats the profile-wide mode warning per project: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), spec.GuestRoot) {
		t.Errorf("preflight announces the guest path per project: %q", stderr.String())
	}
}

// TestPreflightStillReportsWhatDiffersPerProject guards the other direction:
// the metadata warnings genuinely describe this project and must survive.
func TestPreflightStillReportsAMaterialAttribute(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "note.txt")
	if err := os.WriteFile(file, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not a skip on macOS: this is the only test guarding the preserve
	// direction, and a silent skip would let a change that drops real warnings
	// pass as green on a runner that happens not to set attributes.
	if err := setTestXattr(file); err != nil {
		if runtime.GOOS == "darwin" {
			t.Fatalf("setting an extended attribute failed on darwin: %v", err)
		}
		t.Skipf("extended attributes unavailable on %s: %v", runtime.GOOS, err)
	}

	spec, err := broker.BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	coordinator := NewCoordinator(&vm.MockBackend{})
	coordinator.Stderr = &stderr

	if err := coordinator.preflightBroker(&spec); err != nil {
		t.Fatalf("preflightBroker() error = %v", err)
	}
	if !strings.Contains(stderr.String(), "note.txt") {
		t.Errorf("preflight dropped the per-project metadata warning; got %q", stderr.String())
	}
}
