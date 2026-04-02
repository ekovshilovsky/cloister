package vm_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// TestBuildMountsStandardSet verifies that BuildMounts with an auto policy
// returns all standard mounts with the correct paths and writable flags. The
// workspace mount is always the first entry; supplemental mounts follow in
// catalog order.
func TestBuildMountsStandardSet(t *testing.T) {
	homeDir := "/Users/testuser"
	workspaceDir := filepath.Join(homeDir, "code")
	autoPolicy := config.ResourcePolicy{IsSet: true, Mode: "auto"}

	mounts := vm.BuildMounts(homeDir, workspaceDir, nil, autoPolicy, false)

	type expectation struct {
		subpath  string
		writable bool
	}

	want := []expectation{
		{"code", true},
		{".ssh", false},
		{".gnupg", false},
		{"Downloads", false},
		{filepath.Join(".claude", "plugins", "cache"), true},
		{filepath.Join(".claude", "plugins", "marketplaces"), true},
		{filepath.Join(".claude", "skills"), true},
		{filepath.Join(".claude", "agents"), true},
		{".agents", true},
	}

	if len(mounts) != len(want) {
		t.Fatalf("BuildMounts returned %d mounts, want %d", len(mounts), len(want))
	}

	for i, w := range want {
		m := mounts[i]
		wantLoc := filepath.Join(homeDir, w.subpath)
		if m.Location != wantLoc {
			t.Errorf("mounts[%d].Location = %q, want %q", i, m.Location, wantLoc)
		}
		if m.MountPoint != "" {
			t.Errorf("mounts[%d].MountPoint = %q, want empty", i, m.MountPoint)
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
	workspaceDir := filepath.Join(homeDir, "code")
	autoPolicy := config.ResourcePolicy{IsSet: true, Mode: "auto"}

	mounts := vm.BuildMounts(homeDir, workspaceDir, nil, autoPolicy, false)
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
