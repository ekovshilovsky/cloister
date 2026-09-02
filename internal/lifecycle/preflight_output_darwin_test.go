// Proprietary and confidential. All rights reserved.

//go:build darwin

package lifecycle

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"cloister.io/internal/broker"
	"cloister.io/internal/vm"
)

// setTestXattr attaches an extended attribute preflight must report.
//
// Deliberately an unrecognized name rather than a macOS one: an attribute
// describing a file's relationship to this Mac (Spotlight, provenance,
// quarantine) cannot mean anything in the guest, and a preflight that
// distinguishes material attributes from ambient ones should stop reporting
// those. Pinning one here would make this test forbid that fix.
func setTestXattr(path string) error {
	return exec.Command("xattr", "-w", "com.example.novel", "test", path).Run()
}

func TestPreflightReturnsImmaterialSummaryWriteFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "downloaded.txt")
	if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("xattr", "-w", "com.apple.quarantine", "test", path).CombinedOutput(); err != nil {
		t.Fatalf("setting host-relationship xattr: %v: %s", err, output)
	}
	spec, err := broker.BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	diskFull := errors.New("disk full")
	coordinator := NewCoordinator(&vm.MockBackend{})
	coordinator.MetadataLog = &failAfterWriter{successfulWrites: 1, err: diskFull}

	err = coordinator.preflightBroker(&spec)
	if !errors.Is(err, diskFull) {
		t.Fatalf("preflightBroker() error = %v, want immaterial-summary write failure", err)
	}
}

type failAfterWriter struct {
	successfulWrites int
	err              error
}

func (w *failAfterWriter) Write(data []byte) (int, error) {
	if w.successfulWrites == 0 {
		return 0, w.err
	}
	w.successfulWrites--
	return len(data), nil
}
