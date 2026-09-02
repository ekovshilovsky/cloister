// Proprietary and confidential. All rights reserved.

package lifecycle

import (
	"bytes"
	"errors"
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

	if strings.Contains(stderr.String(), "local-filesystem equivalence") {
		t.Errorf("preflight repeats the profile-wide mode warning per project: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), spec.GuestRoot) {
		t.Errorf("preflight announces the guest path per project: %q", stderr.String())
	}
	// macOS stamps com.apple.provenance on ordinary files, so on darwin this
	// project really does carry attributes and this assertion runs against a
	// real filesystem rather than a described one. None of them describe
	// anything but the file's relationship to this Mac, so none of them reach
	// the console.
	for _, hostRelationship := range []string{
		"com.apple.provenance",
		"com.apple.quarantine",
		"com.apple.lastuseddate",
		"com.apple.macl",
		"com.apple.FinderInfo",
		"com.apple.metadata:",
	} {
		if strings.Contains(stderr.String(), hostRelationship) {
			t.Errorf("preflight reports %s, which describes the host rather than the file: %q", hostRelationship, stderr.String())
		}
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

	var stderr, metadataLog bytes.Buffer
	coordinator := NewCoordinator(&vm.MockBackend{})
	coordinator.Stderr = &stderr
	coordinator.MetadataLog = &metadataLog
	coordinator.MetadataLogPath = "/tmp/metadata.log"

	if err := coordinator.preflightBroker(&spec); err != nil {
		t.Fatalf("preflightBroker() error = %v", err)
	}

	// The console names the project, the attribute and how many paths carry it.
	// It no longer names the paths, so these assertions follow the path list to
	// where it went rather than dropping it.
	if !strings.Contains(stderr.String(), "com.example.novel") {
		t.Errorf("preflight dropped the per-project metadata warning; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "1 path") {
		t.Errorf("preflight warning does not say how much of the project is affected; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), coordinator.MetadataLogPath) {
		t.Errorf("preflight warning does not say where the path list is; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Affected paths:") {
		t.Errorf("preflight warning describes directories as files; got %q", stderr.String())
	}
	if !strings.Contains(metadataLog.String(), "note.txt") {
		t.Errorf("the per-path record does not name the affected file; got %q", metadataLog.String())
	}
}

func TestPreflightReturnsMetadataLogWriteFailure(t *testing.T) {
	root := t.TempDir()
	spec, err := broker.BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	diskFull := errors.New("disk full")
	var stderr bytes.Buffer
	coordinator := NewCoordinator(&vm.MockBackend{})
	coordinator.MetadataLog = &lifecycleErrorWriter{err: diskFull}
	coordinator.MetadataLogPath = "/tmp/empty-metadata.log"
	coordinator.Stderr = &stderr

	err = coordinator.preflightBroker(&spec)
	if !errors.Is(err, diskFull) {
		t.Fatalf("preflightBroker() error = %v, want disk-full metadata log failure", err)
	}
	if strings.Contains(stderr.String(), coordinator.MetadataLogPath) {
		t.Fatalf("preflight pointed at an incomplete metadata log: %q", stderr.String())
	}
}

type lifecycleErrorWriter struct {
	err    error
	failed bool
}

func (w *lifecycleErrorWriter) Write(data []byte) (int, error) {
	if !w.failed {
		w.failed = true
		return 0, w.err
	}
	return len(data), nil
}
