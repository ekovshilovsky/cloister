// Proprietary and confidential. All rights reserved.

package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCreatesAProfileScopedLogFile(t *testing.T) {
	dir := t.TempDir()

	run, err := Open(dir, "work", "repair")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer run.Close()

	if _, err := run.Writer().Write([]byte("provisioning output\n")); err != nil {
		t.Fatalf("writing to the run log: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Per-profile directories rather than a filename prefix: profile names
	// contain dashes, so a prefix match would let "battery" prune the logs of
	// "battery-1800".
	if got, want := filepath.Dir(run.Path()), filepath.Join(dir, "work"); got != want {
		t.Errorf("log directory = %q, want %q", got, want)
	}
	name := filepath.Base(run.Path())
	if !strings.HasPrefix(name, "repair-") || !strings.HasSuffix(name, ".log") {
		t.Errorf("log name = %q, want repair-<timestamp>.log", name)
	}
	content, err := os.ReadFile(run.Path())
	if err != nil {
		t.Fatalf("reading the run log: %v", err)
	}
	if string(content) != "provisioning output\n" {
		t.Errorf("log content = %q", content)
	}
}

func TestOpenPrunesToTheRetentionLimit(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Names sort lexically by timestamp, so the oldest sort first.
	existing := []string{
		"repair-20260101-000001.log",
		"repair-20260101-000002.log",
		"create-20260101-000003.log",
		"repair-20260101-000004.log",
	}
	for _, name := range existing {
		if err := os.WriteFile(filepath.Join(profileDir, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	run, err := OpenWithRetention(dir, "work", "repair", 3)
	if err != nil {
		t.Fatalf("OpenWithRetention() error = %v", err)
	}
	defer run.Close()

	remaining, err := filepath.Glob(filepath.Join(profileDir, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 {
		t.Fatalf("kept %d logs, want 3: %v", len(remaining), remaining)
	}
	for _, path := range remaining {
		if strings.Contains(path, "000001") || strings.Contains(path, "000002") {
			t.Errorf("oldest log %q survived pruning", filepath.Base(path))
		}
	}
	// Retention counts every run for the profile, not just runs of the same
	// command, so a create log ages out alongside the repair logs.
	found := false
	for _, path := range remaining {
		if strings.Contains(path, "create-") {
			found = true
		}
	}
	if !found {
		t.Error("a newer create log was pruned ahead of older repair logs")
	}
}

func TestOpenLeavesOtherProfilesAlone(t *testing.T) {
	dir := t.TempDir()
	otherDir := filepath.Join(dir, "innolumi")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(otherDir, "repair-20260101-000001.log")
	if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	run, err := OpenWithRetention(dir, "work", "repair", 1)
	if err != nil {
		t.Fatalf("OpenWithRetention() error = %v", err)
	}
	defer run.Close()

	if _, err := os.Stat(other); err != nil {
		t.Errorf("another profile's log was pruned: %v", err)
	}
}

func TestOpenLeavesUnrelatedFilesAlone(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(profileDir, "notes.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	run, err := OpenWithRetention(dir, "work", "repair", 1)
	if err != nil {
		t.Fatalf("OpenWithRetention() error = %v", err)
	}
	defer run.Close()

	if _, err := os.Stat(keep); err != nil {
		t.Errorf("pruning removed a file it does not own: %v", err)
	}
}

func TestOpenRejectsUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	// The profile and command become path segments, and pruning deletes files
	// in the directory they name, so a traversal here would delete elsewhere.
	for _, testCase := range []struct{ profile, command string }{
		{"../escape", "repair"},
		{"work", "../escape"},
		{"", "repair"},
		{"work", ""},
		{"a/b", "repair"},
	} {
		if run, err := Open(dir, testCase.profile, testCase.command); err == nil {
			run.Close()
			t.Errorf("Open(%q, %q) error = nil, want a refusal", testCase.profile, testCase.command)
		}
	}
}

func TestCloseIsSafeToCallTwice(t *testing.T) {
	run, err := Open(t.TempDir(), "work", "repair")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := run.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil", err)
	}
}

func TestPruneIgnoresFilesItDidNotName(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A .log file without the timestamp Open appends was written by something
	// else, so it is neither pruned nor counted against retention.
	foreign := filepath.Join(profileDir, "someone-elses.log")
	if err := os.WriteFile(foreign, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"repair-20260101-000001.log", "repair-20260101-000002.log"} {
		if err := os.WriteFile(filepath.Join(profileDir, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	run, err := OpenWithRetention(dir, "work", "repair", 2)
	if err != nil {
		t.Fatalf("OpenWithRetention() error = %v", err)
	}
	defer run.Close()

	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("pruning removed a log it did not name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "repair-20260101-000001.log")); !os.IsNotExist(err) {
		t.Error("the oldest owned log survived pruning")
	}
	if _, err := os.Stat(filepath.Join(profileDir, "repair-20260101-000002.log")); err != nil {
		t.Errorf("a log within retention was pruned: %v", err)
	}
}

func TestLogTimestampRecognizesOnlyGeneratedNames(t *testing.T) {
	for _, testCase := range []struct {
		name string
		want bool
	}{
		{"repair-20260101-000001.log", true},
		{"create-20991231-235959.log", true},
		{"someone-elses.log", false},
		{"repair-2026010-000001.log", false},
		{"repair-20260101-00000x.log", false},
		{"repair20260101-000001.log", false},
		{"20260101-000001.log", false},
		{".log", false},
	} {
		if _, ok := logTimestamp(testCase.name); ok != testCase.want {
			t.Errorf("logTimestamp(%q) ok = %v, want %v", testCase.name, ok, testCase.want)
		}
	}
}
