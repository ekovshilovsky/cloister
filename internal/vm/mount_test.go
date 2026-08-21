package vm_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloister.io/internal/config"
	"cloister.io/internal/vm"
)

// TestBuildSupplementalMountsStandardSet verifies the fixed supplemental set
// and proves that no workspace entry is inferred.
func TestBuildSupplementalMountsStandardSet(t *testing.T) {
	homeDir := "/Users/testuser"
	autoPolicy := config.ResourcePolicy{IsSet: true, Mode: "auto"}

	mounts := vm.BuildSupplementalMounts(homeDir, nil, autoPolicy, false)

	type expectation struct {
		subpath  string
		writable bool
	}

	want := []expectation{
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
		t.Fatalf("BuildSupplementalMounts returned %d mounts, want %d", len(mounts), len(want))
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

// TestBuildSupplementalMountsUsesActualHome verifies that the builder respects the home
// directory argument rather than hard-coding any specific path, so that the
// wrapper behaves correctly in non-standard home directory environments.
func TestBuildSupplementalMountsUsesActualHome(t *testing.T) {
	homeDir := t.TempDir()
	autoPolicy := config.ResourcePolicy{IsSet: true, Mode: "auto"}

	mounts := vm.BuildSupplementalMounts(homeDir, nil, autoPolicy, false)
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

func TestBuildSupplementalMountsHeadlessAndOllama(t *testing.T) {
	homeDir := t.TempDir()
	modelsDir := filepath.Join(homeDir, ".ollama", "models")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	autoPolicy := config.ResourcePolicy{IsSet: true, Mode: "auto"}
	mounts := vm.BuildSupplementalMounts(homeDir, []string{"ollama"}, autoPolicy, true)
	for _, mount := range mounts {
		if strings.Contains(mount.Location, ".claude") || strings.HasSuffix(mount.Location, ".agents") {
			if mount.Writable {
				t.Errorf("headless extension mount %q is writable", mount.Location)
			}
		}
	}
	last := mounts[len(mounts)-1]
	if last.Location != modelsDir || last.Writable {
		t.Fatalf("Ollama mount = %#v, want read-only %q", last, modelsDir)
	}
}

func TestMountsChangedDeepComparison(t *testing.T) {
	base := []vm.Mount{{Location: "/host", MountPoint: "/guest", Writable: false}}
	cases := []struct {
		name  string
		after []vm.Mount
		want  bool
	}{
		{name: "identical", after: []vm.Mount{{Location: "/host", MountPoint: "/guest", Writable: false}}, want: false},
		{name: "location", after: []vm.Mount{{Location: "/other", MountPoint: "/guest", Writable: false}}, want: true},
		{name: "mountpoint", after: []vm.Mount{{Location: "/host", MountPoint: "/other", Writable: false}}, want: true},
		{name: "writable", after: []vm.Mount{{Location: "/host", MountPoint: "/guest", Writable: true}}, want: true},
		{name: "length", after: append(append([]vm.Mount(nil), base...), vm.Mount{Location: "/extra"}), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vm.MountsChanged(base, tc.after); got != tc.want {
				t.Fatalf("MountsChanged() = %v, want %v", got, tc.want)
			}
		})
	}
}
