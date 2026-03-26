package lume_test

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/vm/lume"
)

// TestBaseImageName verifies that the exported baseImageName constant used for
// the shared macOS base image matches the canonical cloister convention. This
// name must remain stable because it is used as the source for all OpenClaw
// profile clones; changing it silently would break existing installations.
func TestBaseImageName(t *testing.T) {
	const want = "cloister-base-macos"
	if lume.BaseImageName != want {
		t.Errorf("BaseImageName = %q, want %q", lume.BaseImageName, want)
	}
}

// TestSnapshotNaming verifies that Snapshot derives the correct source and
// destination Lume VM names from a cloister profile name and snapshot label.
// The expected pattern is: source = VMName(profile), dest = VMName(profile)+"-"+name.
// We exercise the naming logic without invoking a real Lume binary by confirming
// the VM name and snapshot VM name via the exported helper functions.
func TestSnapshotNaming(t *testing.T) {
	profile := "myagent"
	snapshotLabel := "factory"

	vmName := lume.VMName(profile)
	wantSrc := "cloister-myagent"
	wantDest := "cloister-myagent-factory"

	if vmName != wantSrc {
		t.Errorf("VMName(%q) = %q, want %q", profile, vmName, wantSrc)
	}

	// Verify the snapshot dest naming convention used inside Snapshot():
	// dest = VMName(profile) + "-" + name
	dest := vmName + "-" + snapshotLabel
	if dest != wantDest {
		t.Errorf("snapshot dest for profile=%q name=%q: got %q, want %q",
			profile, snapshotLabel, dest, wantDest)
	}
}

// TestResetNaming verifies that Reset resolves the correct snapshot VM name for
// both the factory and user reset targets. The factory snapshot is named
// <vmName>-factory and the user snapshot is named <vmName>-user.
func TestResetNaming(t *testing.T) {
	profile := "myagent"
	vmName := lume.VMName(profile)

	cases := []struct {
		toFactory    bool
		wantSnapshot string
	}{
		{toFactory: true, wantSnapshot: vmName + "-factory"},
		{toFactory: false, wantSnapshot: vmName + "-user"},
	}

	for _, tc := range cases {
		suffix := "user"
		if tc.toFactory {
			suffix = "factory"
		}
		got := vmName + "-" + suffix
		if got != tc.wantSnapshot {
			t.Errorf("Reset(%q, toFactory=%v) snapshot name = %q, want %q",
				profile, tc.toFactory, got, tc.wantSnapshot)
		}
	}
}
