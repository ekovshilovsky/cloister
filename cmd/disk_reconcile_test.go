package cmd

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
)

func TestClassifyDrift(t *testing.T) {
	tests := []struct {
		name         string
		actualGB     int64
		configuredGB int
		wantKind     driftKind
	}{
		{"equal: no drift", 40, 40, driftNone},
		{"actual > config: shrink", 80, 40, driftShrink},
		{"actual < config: grow", 20, 40, driftGrow},
		{"config zero treated as no-drift (defaults will apply)", 40, 0, driftNone},
		{"both small: equal", 10, 10, driftNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDrift("root", tc.actualGB, tc.configuredGB)
			if got.kind != tc.wantKind {
				t.Errorf("classifyDrift(actual=%d, configured=%d).kind = %v, want %v",
					tc.actualGB, tc.configuredGB, got.kind, tc.wantKind)
			}
			if got.actualGB != tc.actualGB || got.configuredGB != tc.configuredGB || got.role != "root" {
				t.Errorf("classifyDrift fields not propagated: got %+v", got)
			}
		})
	}
}

// TestDetectDriftForProfile_LegacyLayout exercises the single-disk profile
// shape (only root disk file present, no datadisk in the shared pool) and
// confirms the data-disk absence does not synthesize a phantom drift entry.
// Uses a fake $HOME so the production paths stat real files but in a
// controlled tempdir.
func TestDetectDriftForProfile_LegacyLayout(t *testing.T) {
	home := withFakeHomeForReconcile(t)
	vmName := "colima-cloister-legacy"
	rootPath := filepath.Join(home, ".colima", "_lima", vmName, "disk")
	writeSparseFile(t, rootPath, 80*bytesPerGiB)

	p := &config.Profile{RootDisk: 40, Disk: 80}
	drifts, err := detectDriftForProfile("legacy", p)
	if err != nil {
		t.Fatalf("detectDriftForProfile: %v", err)
	}
	if len(drifts) != 1 {
		t.Fatalf("expected 1 drift (root only, no datadisk file), got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].role != "root" || drifts[0].kind != driftShrink || drifts[0].actualGB != 80 {
		t.Errorf("unexpected drift entry: %+v", drifts[0])
	}
}

// TestDetectDriftForProfile_ModernLayout exercises the dual-disk profile
// shape: both root (in the per-instance dir) and data (in the shared pool)
// disk files exist, with independent sizes.
func TestDetectDriftForProfile_ModernLayout(t *testing.T) {
	home := withFakeHomeForReconcile(t)
	vmName := "colima-cloister-modern"
	rootPath := filepath.Join(home, ".colima", "_lima", vmName, "disk")
	dataPath := filepath.Join(home, ".colima", "_lima", "_disks", vmName, "datadisk")
	writeSparseFile(t, rootPath, 40*bytesPerGiB)
	writeSparseFile(t, dataPath, 80*bytesPerGiB)

	p := &config.Profile{RootDisk: 40, Disk: 40}
	drifts, err := detectDriftForProfile("modern", p)
	if err != nil {
		t.Fatalf("detectDriftForProfile: %v", err)
	}
	if len(drifts) != 2 {
		t.Fatalf("expected 2 drift entries, got %d: %+v", len(drifts), drifts)
	}

	byRole := map[string]diskDrift{}
	for _, d := range drifts {
		byRole[d.role] = d
	}
	if byRole["root"].kind != driftNone {
		t.Errorf("root disk drift = %v, want driftNone (40 == 40)", byRole["root"].kind)
	}
	if byRole["data"].kind != driftShrink || byRole["data"].actualGB != 80 || byRole["data"].configuredGB != 40 {
		t.Errorf("data disk drift = %+v, want shrink 80 vs 40", byRole["data"])
	}
}

// TestReconcileDiskDrift_ShrinkUpdatesConfig drives the shrink-resolution
// path end-to-end: drift detected, user accepts the default Y, profile
// fields are updated, config.Save writes back to the cfgPath file.
func TestReconcileDiskDrift_ShrinkUpdatesConfig(t *testing.T) {
	home := withFakeHomeForReconcile(t)
	vmName := "colima-cloister-shrink"
	rootPath := filepath.Join(home, ".colima", "_lima", vmName, "disk")
	writeSparseFile(t, rootPath, 80*bytesPerGiB)

	cfgPath := filepath.Join(home, ".cloister", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatalf("mkdir cloister: %v", err)
	}
	cfg := &config.Config{
		Profiles: map[string]*config.Profile{
			"shrink": {RootDisk: 40, Disk: 40},
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	// Pipe "y\n" to simulate the operator accepting the default prompt.
	withStdin(t, "y\n")
	withInteractive(t, true)
	var buf bytes.Buffer
	err := reconcileDiskDrift(&buf, cfgPath, cfg, "shrink", cfg.Profiles["shrink"])
	if err != nil {
		t.Fatalf("reconcileDiskDrift: %v\noutput:\n%s", err, buf.String())
	}

	if cfg.Profiles["shrink"].RootDisk != 80 {
		t.Errorf("in-memory RootDisk = %d, want 80", cfg.Profiles["shrink"].RootDisk)
	}

	persisted, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reloading saved config: %v", err)
	}
	if persisted.Profiles["shrink"].RootDisk != 80 {
		t.Errorf("persisted RootDisk = %d, want 80", persisted.Profiles["shrink"].RootDisk)
	}
}

// TestReconcileDiskDrift_DeclineAborts confirms that a user answering N
// halts the start with a clear error instead of silently proceeding to a
// colima call that would fail with "disk shrinking is not supported".
func TestReconcileDiskDrift_DeclineAborts(t *testing.T) {
	home := withFakeHomeForReconcile(t)
	vmName := "colima-cloister-decline"
	rootPath := filepath.Join(home, ".colima", "_lima", vmName, "disk")
	writeSparseFile(t, rootPath, 80*bytesPerGiB)

	cfgPath := filepath.Join(home, ".cloister", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatalf("mkdir cloister: %v", err)
	}
	cfg := &config.Config{
		Profiles: map[string]*config.Profile{
			"decline": {RootDisk: 40, Disk: 40},
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	withStdin(t, "n\n")
	withInteractive(t, true)
	var buf bytes.Buffer
	err := reconcileDiskDrift(&buf, cfgPath, cfg, "decline", cfg.Profiles["decline"])
	if err == nil {
		t.Fatalf("expected error on user decline, got nil. Output:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "aborted by user") {
		t.Errorf("error = %v, want substring 'aborted by user'", err)
	}
	if cfg.Profiles["decline"].RootDisk != 40 {
		t.Errorf("RootDisk mutated despite decline: got %d, want 40", cfg.Profiles["decline"].RootDisk)
	}
}

// TestIsYes encodes the [Y/n] default-accept semantics: empty input or
// anything starting with y/Y is yes; everything else (including 'n', 'no',
// or random text) is no.
func TestIsYes(t *testing.T) {
	yes := []string{"", "\n", "y", "y\n", "Y", "Yes", "yEs\n", "  yes  "}
	no := []string{"n", "n\n", "no", "N", "abc", "0", " 0 "}

	for _, s := range yes {
		if !isYes(s) {
			t.Errorf("isYes(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isYes(s) {
			t.Errorf("isYes(%q) = true, want false", s)
		}
	}
}

// withFakeHomeForReconcile points HOME at a fresh temp directory for the
// duration of the test. Cleanup restores the original HOME so other tests
// (and t.TempDir's own teardown) operate normally.
func withFakeHomeForReconcile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

// writeSparseFile creates a file of the requested size at path, creating
// intermediate directories as needed. The file is sparse-extended via
// Truncate so tests don't actually allocate the requested bytes — useful
// when simulating 80 GiB disk images in a tempdir.
func writeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncate %s to %d: %v", path, size, err)
	}
}

// withStdin replaces os.Stdin with a pipe seeded with the given input for
// the duration of the test. The original stdin is restored on cleanup so
// later tests still see the real stdin.
func withStdin(t *testing.T, input string) {
	t.Helper()
	orig := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatalf("write to pipe: %v", err)
	}
	w.Close()
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		r.Close()
	})
}

// silence the unused-import warning when bufio is only needed transitively;
// the test seam below also serves as a sanity check that bufio.NewReader
// works on os.Stdin in the test environment without needing a real terminal.
var _ = bufio.NewReader(nil)

// withInteractive swaps the TTY-detection hook for the duration of the test
// so the reconciler proceeds to its prompt logic. Without this stub, the
// production isInteractive() reads os.Stdout's mode and sees a non-terminal
// during go test runs, which would short-circuit the prompt path the tests
// are trying to exercise.
func withInteractive(t *testing.T, v bool) {
	t.Helper()
	orig := diskReconcileInteractive
	diskReconcileInteractive = func() bool { return v }
	t.Cleanup(func() { diskReconcileInteractive = orig })
}
