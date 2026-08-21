// Proprietary and confidential. All rights reserved.

package broker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	brokerignore "cloister.io/internal/broker/ignore"
	"cloister.io/internal/vm"
)

type runnerCall struct {
	Env  []string
	Args []string
}

type fakeRunner struct {
	Calls        []runnerCall
	StatusOutput string
	StatusErr    error
}

func (r *fakeRunner) Run(_ context.Context, _ string, env []string, args ...string) ([]byte, error) {
	r.Calls = append(r.Calls, runnerCall{Env: append([]string(nil), env...), Args: append([]string(nil), args...)})
	if len(args) >= 3 && args[0] == "sync" && args[1] == "list" {
		if r.StatusOutput != "" || r.StatusErr != nil {
			return []byte(r.StatusOutput), r.StatusErr
		}
		return []byte("No sessions found\n"), nil
	}
	return []byte("ok\n"), nil
}

type runnerExitError int

func (e runnerExitError) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e runnerExitError) ExitCode() int { return int(e) }

const realMutagenSingleSessionOutput = `--------------------------------------------------------------------------------
Name: cloister-work-0123456789abcdef
Identifier: sync_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789
Alpha:
	URL: /Users/example/project
	Connected: Yes
Beta:
	URL: ssh://cloister-sync-0123456789abcdef/~/workspaces/project-0123456789ab
	Connected: Yes
Conflicts: 0
Status: Watching for changes
--------------------------------------------------------------------------------
`

func TestBuildSessionSpecIsStableAndOpaque(t *testing.T) {
	root := t.TempDir()
	access := vm.SSHAccess{Host: "vm.local", User: "guest"}
	first, err := BuildSessionSpec("Work Profile", root, access, []string{"private/"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildSessionSpec("Work Profile", root, access, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != second.Name || first.GuestRoot != second.GuestRoot || first.ProjectID != second.ProjectID {
		t.Fatalf("session identity is unstable: %#v %#v", first, second)
	}
	if strings.Contains(first.Name, root) || strings.Contains(first.GuestRoot, root) {
		t.Fatalf("session identity leaks host root: %#v", first)
	}
	if !strings.HasPrefix(first.GuestRoot, "~/workspaces/") {
		t.Fatalf("GuestRoot = %q", first.GuestRoot)
	}
}

func TestBuildSessionSpecRejectsSymlinkRoot(t *testing.T) {
	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	_, err := BuildSessionSpec("work", link, vm.SSHAccess{}, nil)
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("BuildSessionSpec() error = %v", err)
	}
}

func TestGuestRootCommandRequiresEmptyFreshTarget(t *testing.T) {
	spec := SessionSpec{GuestRoot: "~/workspaces/project-123"}
	command, err := GuestRootCommand(spec, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "find") || !strings.Contains(command, "$HOME/workspaces/project-123") {
		t.Fatalf("fresh guest command = %q", command)
	}
	resume, err := GuestRootCommand(spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resume, "find") {
		t.Fatalf("resume command unexpectedly requires empty target: %q", resume)
	}
	shell, err := GuestShellCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shell, `cd "$HOME/workspaces/project-123"`) || !strings.Contains(shell, `exec "${SHELL:-/bin/bash}" -l`) {
		t.Fatalf("guest shell command = %q", shell)
	}
	run, err := GuestCommand(spec, "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	if run != `cd "$HOME/workspaces/project-123" && go test ./...` {
		t.Fatalf("guest command = %q", run)
	}
}

func TestMutagenCreateUsesSafeModeAndFinalMandatoryIgnores(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		StatusOutput: `Error: unable to locate requested sessions: specification "cloister-work-missing" did not match any sessions`,
		StatusErr:    runnerExitError(1),
	}
	m := &Mutagen{
		Binary:  "mutagen",
		Runner:  runner,
		DataDir: filepath.Join(t.TempDir(), "data"),
		SSHDir:  filepath.Join(t.TempDir(), "ssh"),
		SSHPath: "/usr/bin/ssh",
		SCPPath: "/usr/bin/scp",
	}
	spec, err := BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest", KeyFile: "/tmp/key"}, []string{"private/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 2 {
		t.Fatalf("calls = %d, want status and create", len(runner.Calls))
	}
	args := runner.Calls[1].Args
	joined := strings.Join(args, " ")
	for _, required := range []string{"sync create", "--sync-mode two-way-safe", "--symlink-mode portable", "--max-entry-count 250000", "--max-staging-file-size 2 GiB", "--probe-mode assume", "--no-global-configuration", "--ignore generated/", "--ignore private/", "--ignore .git", "--ignore node_modules/"} {
		if !strings.Contains(joined, required) {
			t.Errorf("create args missing %q:\n%v", required, args)
		}
	}
	lastIgnore := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--ignore" {
			lastIgnore = args[i+1]
		}
	}
	mandatory := brokerignore.MandatoryPatterns()
	if lastIgnore != mandatory[len(mandatory)-1] {
		t.Fatalf("last ignore = %q, want final mandatory %q", lastIgnore, mandatory[len(mandatory)-1])
	}
	if !containsEnv(runner.Calls[1].Env, "MUTAGEN_DATA_DIRECTORY="+m.DataDir) || !containsEnv(runner.Calls[1].Env, "MUTAGEN_SSH_PATH="+m.SSHDir) {
		t.Fatalf("isolated Mutagen environment missing: %v", runner.Calls[1].Env)
	}
}

func TestMutagenCreateWiresWorkspaceSessionGuardrails(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{
		StatusOutput: `Error: unable to locate requested sessions: specification "missing" did not match any sessions`,
		StatusErr:    runnerExitError(1),
	}
	m := &Mutagen{
		Binary: "mutagen", Runner: runner, DataDir: filepath.Join(t.TempDir(), "data"),
		SSHDir: filepath.Join(t.TempDir(), "ssh"), SSHPath: "/usr/bin/ssh", SCPPath: "/usr/bin/scp",
	}
	spec, err := BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec.MaxEntries = 199_999
	spec.MaxStagingFileSize = "768 MiB"
	spec.ProbeMode = "assume"
	spec.MandatoryIgnore = []string{".git", "node_modules/"}
	if err := m.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.Calls[1].Args, " ")
	for _, required := range []string{"--max-entry-count 199999", "--max-staging-file-size 768 MiB", "--probe-mode assume"} {
		if !strings.Contains(joined, required) {
			t.Errorf("create args missing %q: %s", required, joined)
		}
	}
}

func TestPreflightProjectLimitFailsFast(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "two", "three"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := brokerignore.CompileConfigured(root, nil, []string{".git", "node_modules/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PreflightProjectWithLimit(root, policy, 2); err == nil || !strings.Contains(err.Error(), "maxEntryCount 2") {
		t.Fatalf("PreflightProjectWithLimit() error = %v", err)
	}
}

func TestMutagenRefusesToResumeWithChangedIgnorePolicy(t *testing.T) {
	root := t.TempDir()
	ignorePath := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	m := &Mutagen{
		Binary:  "mutagen",
		Runner:  runner,
		DataDir: filepath.Join(t.TempDir(), "data"),
		SSHDir:  filepath.Join(t.TempDir(), "ssh"),
		SSHPath: "/usr/bin/ssh",
		SCPPath: "/usr/bin/scp",
	}
	spec, err := BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignorePath, []byte("different/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.StatusOutput = "Name: " + spec.Name + "\nStatus: Watching for changes\n"
	if err := m.Create(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "ignore policy changed") {
		t.Fatalf("Create() error = %v", err)
	}

	other := spec
	other.Name = "cloister-other-" + spec.ProjectID
	if m.policyPath(spec) == m.policyPath(other) {
		t.Fatal("policy fingerprints are not isolated by profile-project session")
	}
}

func TestParseMutagenStatusFailsClosed(t *testing.T) {
	status, err := parseMutagenStatus([]byte("Name: work\nAlpha:\n  Connection state: Connected\nBeta:\n  Connection state: Disconnected\nLast error: transport failed\nConflicts:\n  conflict.txt\nStatus: Watching for changes\n"))
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateProblem || status.ConflictCount == 0 || len(status.Problems) == 0 {
		t.Fatalf("status = %#v", status)
	}
	if err := status.Clean(); err == nil {
		t.Fatal("conflicted status reported clean")
	}
	if _, err := parseMutagenStatus([]byte("unexpected output")); err == nil {
		t.Fatal("unrecognized status did not fail closed")
	}
}

func TestMutagenStatusReportsMissingForExitOneNoMatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
	}{
		{
			name:   "not found",
			output: `Error: unable to locate requested sessions: specification "cloister-work-missing" did not match any sessions`,
		},
		{
			name: "daemon autostart banner",
			output: "Attempting to start Mutagen daemon...\n" +
				`Error: unable to locate requested sessions: specification "cloister-work-missing" did not match any sessions` + "\n" +
				"Started Mutagen daemon in background",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{StatusOutput: test.output, StatusErr: runnerExitError(1)}
			m := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: t.TempDir()}
			status, err := m.Status(context.Background(), SessionSpec{Name: "cloister-work-missing"})
			if err != nil {
				t.Fatal(err)
			}
			if status.State != StateMissing {
				t.Fatalf("Status() = %#v, want StateMissing", status)
			}
		})
	}
}

func TestParseMutagenStatusHandlesRealSingleSessionOutput(t *testing.T) {
	status, err := parseMutagenStatus([]byte(realMutagenSingleSessionOutput))
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateActive || status.Description != "Watching for changes" || status.ConflictCount != 0 || len(status.Problems) != 0 {
		t.Fatalf("status = %#v", status)
	}
}

func TestMutagenStatusIgnoresDaemonAutostartBannerWithValidOutput(t *testing.T) {
	runner := &fakeRunner{
		StatusOutput: "Attempting to start Mutagen daemon...\n" + realMutagenSingleSessionOutput + "Started Mutagen daemon in background\n",
	}
	m := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: t.TempDir()}
	status, err := m.Status(context.Background(), SessionSpec{Name: "cloister-work-0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateActive || status.ConflictCount != 0 {
		t.Fatalf("Status() = %#v", status)
	}
}

func TestMutagenStatusPreservesGenuineErrors(t *testing.T) {
	runner := &fakeRunner{
		StatusOutput: "Attempting to start Mutagen daemon...\nunable to connect to daemon: transport is closing",
		StatusErr:    runnerExitError(1),
	}
	m := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: t.TempDir()}
	status, err := m.Status(context.Background(), SessionSpec{Name: "cloister-work-existing"})
	if err == nil || !strings.Contains(err.Error(), "transport is closing") {
		t.Fatalf("Status() status = %#v, error = %v", status, err)
	}
}

func TestMutagenTerminateTreatsMissingSessionAsAlreadyTerminated(t *testing.T) {
	runner := &fakeRunner{
		StatusOutput: `Error: unable to locate requested sessions: specification "cloister-work-missing" did not match any sessions`,
		StatusErr:    runnerExitError(1),
	}
	m := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: t.TempDir()}
	if err := m.Terminate(context.Background(), SessionSpec{Name: "cloister-work-missing"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 1 || !reflect.DeepEqual(runner.Calls[0].Args, []string{"sync", "list", "--long", "cloister-work-missing"}) {
		t.Fatalf("calls = %#v, want only status check", runner.Calls)
	}
}

func TestMissingMutagenErrorIsActionable(t *testing.T) {
	message := missingMutagenError().Error()
	for _, text := range []string{"not found in PATH", "brew install mutagen-io/mutagen/mutagen", "will not install"} {
		if !strings.Contains(message, text) {
			t.Errorf("missing Mutagen error lacks %q: %s", text, message)
		}
	}
}

func TestMutagenHoldsNoPerFileBookkeeping(t *testing.T) {
	typeOfMutagen := reflect.TypeOf(Mutagen{})
	for i := 0; i < typeOfMutagen.NumField(); i++ {
		field := typeOfMutagen.Field(i)
		if field.Type.Kind() == reflect.Map || field.Type.Kind() == reflect.Slice {
			t.Fatalf("Mutagen field %s introduces variable per-entry bookkeeping: %s", field.Name, field.Type)
		}
	}

	root := t.TempDir()
	for i := 0; i < 1000; i++ {
		name := filepath.Join(root, "file-"+strconv.Itoa(i))
		if err := os.WriteFile(name, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := brokerignore.Compile(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := openFDCount(t)
	if _, err := preflightProject(root, policy, noXattrInspector{}); err != nil {
		t.Fatal(err)
	}
	after := openFDCount(t)
	if after > before+4 {
		t.Fatalf("preflight leaked descriptors: before=%d after=%d", before, after)
	}
	t.Log("This unit assertion covers Cloister data structures and preflight descriptor cleanup. Bounded Mutagen process fd growth requires a real Mutagen and VM end-to-end test.")
}

func TestPreflightRejectsHardlinksAndEscapingSymlinks(t *testing.T) {
	t.Run("hardlink", func(t *testing.T) {
		root := t.TempDir()
		first := filepath.Join(root, "first")
		if err := os.WriteFile(first, []byte("same inode"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(first, filepath.Join(root, "second")); err != nil {
			t.Fatal(err)
		}
		policy, err := brokerignore.Compile(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = preflightProject(root, policy, noXattrInspector{})
		if err == nil || !strings.Contains(err.Error(), "hardlinked file") {
			t.Fatalf("preflight error = %v", err)
		}
	})

	t.Run("symlink escape", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink("../outside", filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		policy, err := brokerignore.Compile(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = preflightProject(root, policy, noXattrInspector{})
		if err == nil || !strings.Contains(err.Error(), "portable relative symlinks") {
			t.Fatalf("preflight error = %v", err)
		}
	})
}

func TestPreflightWarnsForXattrsAndSkipsMandatoryTrees(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "source.go")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ignored := filepath.Join(root, "node_modules")
	if err := os.MkdirAll(ignored, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(ignored, "a")
	if err := os.WriteFile(first, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(ignored, "b")); err != nil {
		t.Fatal(err)
	}
	policy, err := brokerignore.Compile(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := preflightProject(root, policy, fixedXattrInspector{path: file})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "com.example.material") {
		t.Fatalf("warnings = %v", report.Warnings)
	}
}

type noXattrInspector struct{}

func (noXattrInspector) Xattrs(string) ([]string, error) { return nil, nil }

type fixedXattrInspector struct{ path string }

func (i fixedXattrInspector) Xattrs(path string) ([]string, error) {
	if filepath.Base(path) == filepath.Base(i.path) {
		return []string{"com.example.material"}, nil
	}
	return nil, nil
}

func containsEnv(env []string, value string) bool {
	for _, entry := range env {
		if entry == value {
			return true
		}
	}
	return false
}

func openFDCount(t *testing.T) int {
	t.Helper()
	directory, err := os.Open("/dev/fd")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/dev/fd is unavailable")
		}
		t.Fatal(err)
	}
	defer directory.Close()
	names, err := directory.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}
