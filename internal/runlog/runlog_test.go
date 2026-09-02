// Proprietary and confidential. All rights reserved.

package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		{"repair-20260101-000001.123456789.log", true},
		{"repair-20260101-000001.123456789-001.log", true},
		{"someone-elses.log", false},
		{"repair-2026010-000001.log", false},
		{"repair-20260101-00000x.log", false},
		{"repair-20260101-000001-001.log", false},
		{"repair-20260101-000001.123456789-000.log", false},
		{"repair-20260101-000001.123456789-abc.log", false},
		{"repair20260101-000001.log", false},
		{"20260101-000001.log", false},
		{".log", false},
	} {
		if _, ok := logTimestamp(testCase.name); ok != testCase.want {
			t.Errorf("logTimestamp(%q) ok = %v, want %v", testCase.name, ok, testCase.want)
		}
	}
}

// Opening a run log must never destroy another run's log. No sleep belongs
// between these opens because back-to-back commands are the required case.
func TestOpenDoesNotCollideWithinTheSameSecond(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir, "work", "repair")
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	defer first.Close()
	if _, err := first.Writer().Write([]byte("first run\n")); err != nil {
		t.Fatalf("writing the first run log: %v", err)
	}

	second, err := Open(dir, "work", "repair")
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer second.Close()
	if _, err := second.Writer().Write([]byte("second run\n")); err != nil {
		t.Fatalf("writing the second run log: %v", err)
	}

	if first.Path() == second.Path() {
		t.Fatalf("both runs opened the same log %q", first.Path())
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	for _, run := range []struct {
		path string
		want string
	}{{first.Path(), "first run\n"}, {second.Path(), "second run\n"}} {
		content, err := os.ReadFile(run.path)
		if err != nil {
			t.Fatalf("reading %q: %v", run.path, err)
		}
		if string(content) != run.want {
			t.Errorf("log %q = %q, want %q", run.path, content, run.want)
		}
	}
}

// Concurrent opens must resolve to independent files without shared state, and
// every one of them must survive the pruning the others do on their way in.
//
// Retention is deliberately below the number of concurrent runs. Turning it off
// would leave the create path tested and the interesting half -- creation racing
// pruning, which is the half that destroys a log -- untested.
func TestOpenIsSafeWhenRunsRaceToCreateALog(t *testing.T) {
	dir := t.TempDir()
	const runs = 16
	const retention = 4

	opened := make([]*Run, runs)
	errs := make([]error, runs)
	var wait sync.WaitGroup
	wait.Add(runs)
	for i := 0; i < runs; i++ {
		go func(i int) {
			defer wait.Done()
			run, err := OpenWithRetention(dir, "work", "repair", retention)
			if err != nil {
				errs[i] = err
				return
			}
			opened[i] = run
			_, errs[i] = fmt.Fprintf(run.Writer(), "run %d\n", i)
		}(i)
	}
	wait.Wait()

	// Every run is still open at this point, so every log must still be there
	// and hold what its run wrote.
	seen := make(map[string]bool, runs)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		path := opened[i].Path()
		if seen[path] {
			t.Fatalf("run %d reused log path %q", i, path)
		}
		seen[path] = true
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("run %d: reading %q: %v", i, path, err)
		}
		if want := fmt.Sprintf("run %d\n", i); string(content) != want {
			t.Errorf("log %q = %q, want %q", path, content, want)
		}
	}

	for i, run := range opened {
		if err := run.Close(); err != nil {
			t.Errorf("run %d: Close() error = %v", i, err)
		}
	}
}

// Retention applies to every persisted timestamp format owned by this package.
func TestPruneStillRecognizesLogsWrittenByEarlierVersions(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []string{
		"repair-20260101-000001.log",
		"repair-20260101-000002.log",
	}
	for _, name := range legacy {
		if err := os.WriteFile(filepath.Join(profileDir, name), []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Retention of two: the new run plus one survivor, so the older legacy log
	// must be pruned. A parser blind to the legacy stamp would keep both.
	run, err := OpenWithRetention(dir, "work", "repair", 2)
	if err != nil {
		t.Fatalf("OpenWithRetention() error = %v", err)
	}
	defer run.Close()

	remaining, err := filepath.Glob(filepath.Join(profileDir, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("after pruning, %d logs remain, want 2: %v", len(remaining), remaining)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "repair-20260101-000001.log")); !os.IsNotExist(err) {
		t.Errorf("the oldest legacy log survived pruning (stat err = %v)", err)
	}
}

// A fixed stamp makes the exclusive-create invariant deterministic.
func TestCreateRunNeverReusesAFileForTheSameStamp(t *testing.T) {
	profileDir := t.TempDir()
	const stamp = "20260101-120000.000000000"

	first, err := createRun(profileDir, "repair", stamp)
	if err != nil {
		t.Fatalf("first createRun() error = %v", err)
	}
	defer first.Close()
	if _, err := first.Writer().Write([]byte("first run\n")); err != nil {
		t.Fatal(err)
	}

	second, err := createRun(profileDir, "repair", stamp)
	if err != nil {
		t.Fatalf("second createRun() error = %v", err)
	}
	defer second.Close()
	if _, err := second.Writer().Write([]byte("second run\n")); err != nil {
		t.Fatal(err)
	}

	if first.Path() == second.Path() {
		t.Fatalf("both runs opened the same log %q", first.Path())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	for _, run := range []struct{ path, want string }{
		{first.Path(), "first run\n"},
		{second.Path(), "second run\n"},
	} {
		content, err := os.ReadFile(run.path)
		if err != nil {
			t.Fatalf("reading %q: %v", run.path, err)
		}
		if string(content) != run.want {
			t.Errorf("log %q = %q, want %q", run.path, content, run.want)
		}
	}
}

// Collision suffixes remain part of the owned format used by pruning.
func TestPruneRecognizesACollisionBrokenName(t *testing.T) {
	profileDir := t.TempDir()
	const stamp = "20260101-120000.000000000"

	run, err := createRun(profileDir, "repair", stamp)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	collided, err := createRun(profileDir, "repair", stamp)
	if err != nil {
		t.Fatal(err)
	}
	defer collided.Close()

	name := filepath.Base(collided.Path())
	got, ok := logTimestamp(name)
	if !ok {
		t.Fatalf("logTimestamp(%q) did not recognize a name this package wrote", name)
	}
	if got != stamp {
		t.Errorf("logTimestamp(%q) = %q, want %q", name, got, stamp)
	}
}

// Retention makes room among finished runs, never among running ones. A run
// whose log has been unlinked keeps writing into a file nobody can open, so the
// command reports a path that resolves to nothing.
func TestOpenDoesNotPruneARunThatIsStillOpen(t *testing.T) {
	dir := t.TempDir()

	// Two more concurrent runs than retention allows: without protection the
	// first two are exactly the ones pruning chooses.
	open := make([]*Run, 0, DefaultRetention+2)
	for i := 0; i < DefaultRetention+2; i++ {
		run, err := Open(dir, "work", "repair")
		if err != nil {
			t.Fatalf("run %d: Open() error = %v", i, err)
		}
		defer run.Close()
		open = append(open, run)
	}

	for i, run := range open {
		if _, err := os.Stat(run.Path()); err != nil {
			t.Errorf("run %d was pruned while still being written: %v", i, err)
		}
	}
}

// Protecting an open run must not exempt it forever: once it is closed it is an
// ordinary old log and retention applies to it.
func TestARunBecomesPrunableOnceItIsClosed(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "work")

	first, err := OpenWithRetention(dir, "work", "repair", 2)
	if err != nil {
		t.Fatal(err)
	}
	firstPath := first.Path()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Two later runs, so retention of two has to drop the closed first one.
	for i := 0; i < 2; i++ {
		run, err := OpenWithRetention(dir, "work", "repair", 2)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		remaining, _ := filepath.Glob(filepath.Join(profileDir, "*.log"))
		t.Errorf("closed run %q survived retention; directory holds %v", filepath.Base(firstPath), remaining)
	}
}
