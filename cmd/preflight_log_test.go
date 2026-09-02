package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloister.io/internal/lifecycle"
	"cloister.io/internal/vm"
)

// TestAttachPreflightLogKeepsTheFileListReadable covers the half of the change
// that keeps it a relocation rather than a deletion: the console reports counts,
// and the files behind those counts have to land somewhere a reader can open.
func TestAttachPreflightLogKeepsTheFileListReadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	coordinator := lifecycle.NewCoordinator(&vm.MockBackend{})
	release := attachPreflightLog(coordinator, "work")
	defer release()

	if coordinator.MetadataLog == nil {
		t.Fatal("coordinator has nowhere to record the per-file detail")
	}
	if coordinator.MetadataLogPath == "" {
		t.Fatal("coordinator cannot tell the reader where the detail went")
	}

	if _, err := io.WriteString(coordinator.MetadataLog, "note.txt has host-only extended attributes: user.test\n"); err != nil {
		t.Fatalf("writing detail: %v", err)
	}
	release()

	recorded, err := os.ReadFile(coordinator.MetadataLogPath)
	if err != nil {
		t.Fatalf("reading the recorded detail: %v", err)
	}
	if !strings.Contains(string(recorded), "note.txt") {
		t.Errorf("the record does not carry the detail: %q", recorded)
	}
}

// TestPreflightLogCannotEvictProvisioningLogs pins the directory choice. Run
// log retention is per directory and an entry happens many times for every
// provision, so one shared budget would quietly discard the provisioning
// records that a failed create needs.
func TestPreflightLogCannotEvictProvisioningLogs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	coordinator := lifecycle.NewCoordinator(&vm.MockBackend{})
	release := attachPreflightLog(coordinator, "work")
	defer release()

	provisioning, err := logDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Dir(coordinator.MetadataLogPath); got == filepath.Join(provisioning, "work") {
		t.Fatalf("preflight logs share the provisioning retention budget at %q", got)
	}
	if got, want := filepath.Dir(coordinator.MetadataLogPath), filepath.Join(provisioning, "workspace", "work"); got != want {
		t.Errorf("preflight log directory = %q, want %q", got, want)
	}
}
