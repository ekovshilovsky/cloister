package vm_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/vm"
)

// TestVMName verifies that VMName correctly prepends the cloister prefix to a
// profile name, producing the Colima instance name used internally.
func TestVMName(t *testing.T) {
	cases := []struct {
		profile string
		want    string
	}{
		{"work", "cloister-work"},
		{"dev", "cloister-dev"},
		{"personal", "cloister-personal"},
		{"", "cloister-"},
	}

	for _, tc := range cases {
		got := vm.VMName(tc.profile)
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
		{"cloister-work", "work"},
		{"cloister-dev", "dev"},
		{"cloister-personal", "personal"},
		// VM names that do not carry the cloister prefix must return empty string.
		{"colima-default", ""},
		{"default", ""},
		{"", ""},
		// A string equal to the bare prefix with no profile segment following it.
		{"cloister-", ""},
	}

	for _, tc := range cases {
		got := vm.ProfileFromVMName(tc.vmName)
		if got != tc.want {
			t.Errorf("ProfileFromVMName(%q) = %q, want %q", tc.vmName, got, tc.want)
		}
	}
}

// TestBuildMounts verifies that BuildMounts returns the standard set of host
// directory mounts with the correct paths and writable flags for a given home
// directory.
func TestBuildMounts(t *testing.T) {
	homeDir := "/Users/testuser"

	mounts := vm.BuildMounts(homeDir)

	// Expected mount table: location, mountPoint (empty = same as location), writable.
	type expectation struct {
		location   string
		mountPoint string
		writable   bool
	}

	want := []expectation{
		{filepath.Join(homeDir, "Code"), "", true},
		{filepath.Join(homeDir, ".ssh"), "", false},
		{filepath.Join(homeDir, ".gnupg"), "", false},
		{filepath.Join(homeDir, "Downloads"), "", false},
		{filepath.Join(homeDir, ".claude", "plugins"), "", true},
		{filepath.Join(homeDir, ".claude", "skills"), "", true},
		{filepath.Join(homeDir, ".claude", "agents"), "", true},
	}

	if len(mounts) != len(want) {
		t.Fatalf("BuildMounts returned %d mounts, want %d", len(mounts), len(want))
	}

	for i, w := range want {
		m := mounts[i]
		if m.Location != w.location {
			t.Errorf("mounts[%d].Location = %q, want %q", i, m.Location, w.location)
		}
		if m.MountPoint != w.mountPoint {
			t.Errorf("mounts[%d].MountPoint = %q, want %q", i, m.MountPoint, w.mountPoint)
		}
		if m.Writable != w.writable {
			t.Errorf("mounts[%d].Writable = %v, want %v", i, m.Writable, w.writable)
		}
	}
}

// TestBuildMountsUsesActualHome verifies that BuildMounts respects the home
// directory argument rather than hard-coding any specific path, so that the
// wrapper behaves correctly in non-standard home directory environments.
func TestBuildMountsUsesActualHome(t *testing.T) {
	homeDir := t.TempDir()

	mounts := vm.BuildMounts(homeDir)
	for _, m := range mounts {
		if !filepath.IsAbs(m.Location) {
			t.Errorf("mount location %q is not an absolute path", m.Location)
		}
		rel, err := filepath.Rel(homeDir, m.Location)
		if err != nil || (len(rel) >= 2 && rel[:2] == "..") {
			t.Errorf("mount location %q is not under homeDir %q", m.Location, homeDir)
		}
		_ = os.MkdirAll(m.Location, 0o700)
	}
}
