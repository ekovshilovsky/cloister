package colima

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// requireDarwin skips the calling test on non-darwin platforms. The disk
// resize path uses /bin/cp -c (macOS APFS clonefile(2)) which is not
// portable to the Linux runners used by CI. Cloister itself only runs on
// macOS, so the resize path is exercised exclusively in the darwin build.
func requireDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skipf("skipping: resize path uses /bin/cp -c (APFS clonefile), darwin-only")
	}
}

func withFakeHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	prev := userHomeDir
	userHomeDir = func() (string, error) { return tmp, nil }
	t.Cleanup(func() { userHomeDir = prev })
	return tmp
}

func writeColimaYAML(t *testing.T, home, profile, body string) {
	t.Helper()
	dir := filepath.Join(home, ".colima", VMName(profile))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir colima dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "colima.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing colima.yaml: %v", err)
	}
}

func writeLimaYAML(t *testing.T, home, profile, body string) {
	t.Helper()
	dir := filepath.Join(home, ".colima", "_lima", "colima-"+VMName(profile))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir lima dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lima.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing lima.yaml: %v", err)
	}
}

func writeRawDiskOfSize(t *testing.T, home, profile string, sizeBytes int64) {
	t.Helper()
	dir := filepath.Join(home, ".colima", "_lima", "colima-"+VMName(profile))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir lima dir: %v", err)
	}
	path := filepath.Join(dir, "disk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create disk: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(sizeBytes); err != nil {
		t.Fatalf("truncating fake disk: %v", err)
	}
}

func TestRootDiskGB(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"present", "cpu: 4\nrootDisk: 40\nmemory: 4\n", 40},
		{"absent", "cpu: 4\nmemory: 4\n", 0},
		{"with trailing comment ignored by regex", "rootDisk: 20 # was 40 once\n", 0},
		{"twenty literal", "rootDisk: 20\n", 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := withFakeHome(t)
			writeColimaYAML(t, home, "p1", tt.body)
			got, err := RootDiskGB("p1")
			if err != nil {
				t.Fatalf("RootDiskGB: %v", err)
			}
			if got != tt.want {
				t.Errorf("RootDiskGB = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRootDiskGB_MissingProfile(t *testing.T) {
	withFakeHome(t)
	got, err := RootDiskGB("nonexistent")
	if err != nil {
		t.Fatalf("expected nil error for missing profile, got %v", err)
	}
	if got != 0 {
		t.Errorf("expected 0 for missing profile, got %d", got)
	}
}

func TestResizeRootDiskFile_GrowsAndUpdatesYAMLs(t *testing.T) {
	requireDarwin(t)
	home := withFakeHome(t)
	writeColimaYAML(t, home, "p1", "cpu: 4\nrootDisk: 20\nmemory: 4\n")
	writeLimaYAML(t, home, "p1", "cpu: 4\ndisk: 20GiB\nmemory: 4GiB\n")
	writeRawDiskOfSize(t, home, "p1", 1<<20) // 1 MiB starting size

	if err := ResizeRootDiskFile("p1", 2); err != nil {
		t.Fatalf("ResizeRootDiskFile: %v", err)
	}

	diskPath := filepath.Join(home, ".colima", "_lima", "colima-"+VMName("p1"), "disk")
	fi, err := os.Stat(diskPath)
	if err != nil {
		t.Fatalf("stat disk: %v", err)
	}
	wantBytes := int64(2) * 1024 * 1024 * 1024
	if fi.Size() != wantBytes {
		t.Errorf("disk size = %d, want %d", fi.Size(), wantBytes)
	}

	gb, err := RootDiskGB("p1")
	if err != nil {
		t.Fatalf("RootDiskGB: %v", err)
	}
	if gb != 2 {
		t.Errorf("colima.yaml rootDisk = %d, want 2", gb)
	}

	limaPath := filepath.Join(home, ".colima", "_lima", "colima-"+VMName("p1"), "lima.yaml")
	limaData, err := os.ReadFile(limaPath)
	if err != nil {
		t.Fatalf("reading lima.yaml: %v", err)
	}
	if !limaDiskRE.Match(limaData) || string(limaDiskRE.FindSubmatch(limaData)[1]) != "2" {
		t.Errorf("lima.yaml disk not updated to 2GiB; got:\n%s", limaData)
	}

	backupPath := filepath.Join(home, ".colima", "_lima", "colima-"+VMName("p1"), "disk.bak")
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("expected disk.bak to exist after resize, got %v", err)
	}
	if err := CleanupResizeBackup("p1"); err != nil {
		t.Errorf("CleanupResizeBackup: %v", err)
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Errorf("expected disk.bak removed, got err=%v", err)
	}
}

func TestResizeRootDiskFile_RefusesShrink(t *testing.T) {
	requireDarwin(t)
	home := withFakeHome(t)
	writeColimaYAML(t, home, "p1", "rootDisk: 40\n")
	writeLimaYAML(t, home, "p1", "disk: 40GiB\n")
	writeRawDiskOfSize(t, home, "p1", int64(2)*1024*1024*1024)

	err := ResizeRootDiskFile("p1", 1)
	if err == nil {
		t.Fatalf("expected error on shrink request, got nil")
	}
}
