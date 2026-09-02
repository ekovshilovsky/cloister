package broker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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
	Calls         []runnerCall
	StatusOutput  string
	StatusErr     error
	CommandErrors map[string]error
	CommandOutput map[string]string
}

func (r *fakeRunner) Run(_ context.Context, _ string, env []string, args ...string) ([]byte, error) {
	r.Calls = append(r.Calls, runnerCall{Env: append([]string(nil), env...), Args: append([]string(nil), args...)})
	key := ""
	if len(args) >= 2 {
		key = strings.Join(args[:2], " ")
	}
	if err := r.CommandErrors[key]; err != nil {
		return []byte(r.CommandOutput[key]), err
	}
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

// liveSessionOutput renders the `mutagen sync list --long` output for one
// healthy session synchronizing to the given guest path. Tests that exercise
// session recreation need a beta endpoint in the fixture: recreation decisions
// depend on where the live session actually points.
func liveSessionOutput(name, hostRoot, guestRoot string) string {
	return "Name: " + name + "\n" +
		"Alpha:\n\tURL: " + hostRoot + "\n\tConnected: Yes\n" +
		"Beta:\n\tURL: vm.local:" + guestRoot + "\n\tConnected: Yes\n" +
		"Status: Watching for changes\n"
}

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

// A paused Mutagen session reports both endpoints as unconnected because the
// daemon intentionally drops its transports while paused.
const realMutagenPausedSessionOutput = `--------------------------------------------------------------------------------
Name: cloister-local-dev-222222222222222222222222
Identifier: sync_ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210
Labels: None
Alpha:
	URL: /tmp/example/project
	Connected: No
Beta:
	URL: ssh://example-guest/~/workspaces/project-222222222222
	Connected: No
Status: [Paused]
--------------------------------------------------------------------------------
`

// activeMutagenSessionOutput renders the Mutagen 0.18.1 `sync list --long`
// shape for a connected session. Empty conflict lists are omitted the way
// Mutagen omits them.
func activeMutagenSessionOutput(description string) string {
	return "--------------------------------------------------------------------------------\n" +
		"Name: cloister-test-profile-0123456789abcdef\n" +
		"Identifier: sync_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789\n" +
		"Alpha:\n\tURL: /Users/example/project\n\tConnected: Yes\n" +
		"Beta:\n\tURL: ssh://cloister-sync-0123456789abcdef/~/workspaces/project-0123456789ab\n\tConnected: Yes\n" +
		"Status: " + description + "\n" +
		"--------------------------------------------------------------------------------\n"
}

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

func TestBuildSessionSpecUsesNavigableGuestRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "clients", "acme account", "API Service")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	spec, err := BuildSessionSpec("work", root, vm.SSHAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "~/workspaces/api-service-acme-account--" + spec.ProjectID
	if spec.GuestRoot != want {
		t.Fatalf("GuestRoot = %q, want %q", spec.GuestRoot, want)
	}
}

func TestBuildSessionSpecGuestRootsDoNotAliasNestedProjects(t *testing.T) {
	root := t.TempDir()
	projectOne := filepath.Join(root, "one", "account", "api")
	projectTwo := filepath.Join(root, "two", "account", "api")
	for _, project := range []string{projectOne, projectTwo} {
		if err := os.MkdirAll(project, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	one, err := BuildSessionSpec("work", projectOne, vm.SSHAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := BuildSessionSpec("work", projectTwo, vm.SSHAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if one.ProjectID == two.ProjectID {
		t.Fatalf("test projects unexpectedly share ProjectID %q", one.ProjectID)
	}
	if one.GuestRoot == two.GuestRoot {
		t.Fatalf("distinct projects share guest root %q", one.GuestRoot)
	}
	for _, spec := range []SessionSpec{one, two} {
		if !strings.Contains(spec.GuestRoot, "api-account") || !strings.Contains(spec.GuestRoot, spec.ProjectID) {
			t.Errorf("GuestRoot %q does not preserve its readable name and complete project identity", spec.GuestRoot)
		}
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
	spec := SessionSpec{ProjectID: strings.Repeat("1", 24), GuestRoot: "~/workspaces/project-123"}
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

func TestValidateSessionSpecsRejectsGuestRootAliases(t *testing.T) {
	shared := "~/workspaces/apps/api"
	err := ValidateSessionSpecs([]SessionSpec{
		{ProjectID: strings.Repeat("1", 24), HostRoot: "/host/one", GuestRoot: shared},
		{ProjectID: strings.Repeat("2", 24), HostRoot: "/host/two", GuestRoot: shared},
	})
	if err == nil || !strings.Contains(err.Error(), "both claim guest path") {
		t.Fatalf("ValidateSessionSpecs() error = %v", err)
	}
}

func TestGuestRootResetRefusesAnotherProjectClaim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	owner := SessionSpec{ProjectID: strings.Repeat("1", 24), GuestRoot: "~/workspaces/project"}
	prepare, err := GuestRootCommand(owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-c", prepare).CombinedOutput(); err != nil {
		t.Fatalf("creating owner claim: %v: %s", err, output)
	}
	sentinel := filepath.Join(home, "workspaces", "project", "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	intruder := owner
	intruder.ProjectID = strings.Repeat("2", 24)
	reset, err := GuestRootResetCommand(intruder)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-c", reset).CombinedOutput(); err == nil || !strings.Contains(string(output), "different project") {
		t.Fatalf("reset command error = %v, output = %q", err, output)
	}
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "keep" {
		t.Fatalf("claimed guest root was modified: contents=%q err=%v", contents, err)
	}
}

func TestGuestRootResetRefusesUnclaimedTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	spec := SessionSpec{ProjectID: strings.Repeat("7", 24), GuestRoot: "~/workspaces/unclaimed"}
	target := filepath.Join(home, "workspaces", "unclaimed")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(target, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	reset, err := GuestRootResetCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-c", reset).CombinedOutput(); err == nil || !strings.Contains(string(output), "ownership is incomplete") {
		t.Fatalf("reset command error = %v, output = %q", err, output)
	}
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "keep" {
		t.Fatalf("unclaimed guest root was modified: contents=%q err=%v", contents, err)
	}
}

func TestFreshGuestRootQuarantinesNonEmptyDestination(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	spec := SessionSpec{ProjectID: strings.Repeat("3", 24), GuestRoot: "~/workspaces/project"}
	target := filepath.Join(home, "workspaces", "project")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "unverified"), []byte("retain"), 0o600); err != nil {
		t.Fatal(err)
	}

	command, err := GuestRootCommand(spec, true)
	if err != nil {
		t.Fatal(err)
	}
	output, runErr := exec.Command("sh", "-c", command).CombinedOutput()
	if runErr == nil || !strings.Contains(string(output), "quarantined for review") {
		t.Fatalf("fresh-root command error = %v, output = %q", runErr, output)
	}
	quarantined := filepath.Join(home, ".cloister", "quarantine", "guest-roots", "workspaces", "project.quarantine", "unverified")
	if contents, err := os.ReadFile(quarantined); err != nil || string(contents) != "retain" {
		t.Fatalf("quarantined contents = %q, err = %v", contents, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("non-empty destination still exists after quarantine: %v", err)
	}

	output, runErr = exec.Command("sh", "-c", command).CombinedOutput()
	if runErr == nil || !strings.Contains(string(output), "requires review") {
		t.Fatalf("retry bypassed pending quarantine: error = %v, output = %q", runErr, output)
	}
}

func TestGuestRootRemoveRefusesInvalidOwnershipRecords(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		state string
	}{
		{name: "missing", state: "missing"},
		{name: "empty", state: "empty"},
		{name: "corrupt", state: "not-a-project-id"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			spec := SessionSpec{ProjectID: strings.Repeat("4", 24), GuestRoot: "~/workspaces/old-project"}
			root := filepath.Join(home, "workspaces", "old-project")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(root, "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			owner := filepath.Join(home, ".cloister", "guest-root-owners", "workspaces", "old-project.owner")
			if testCase.state != "missing" {
				if err := os.MkdirAll(owner, 0o700); err != nil {
					t.Fatal(err)
				}
				contents := ""
				if testCase.state == "not-a-project-id" {
					contents = testCase.state + "\n"
				}
				if err := os.WriteFile(filepath.Join(owner, "project-id"), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			remove, err := GuestRootRemoveCommand(spec)
			if err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("sh", "-c", remove).CombinedOutput(); err == nil {
				t.Fatalf("removal succeeded with %s ownership record: %s", testCase.state, output)
			}
			if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "keep" {
				t.Fatalf("refused removal modified tree: contents=%q err=%v", contents, err)
			}
		})
	}
}

func TestGuestRootRemoveRefusesDifferentProjectOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ownerSpec := SessionSpec{ProjectID: strings.Repeat("5", 24), GuestRoot: "~/workspaces/old-project"}
	prepare, err := GuestRootCommand(ownerSpec, false)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-c", prepare).CombinedOutput(); err != nil {
		t.Fatalf("preparing owner claim: %v: %s", err, output)
	}
	sentinel := filepath.Join(home, "workspaces", "old-project", "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	other := ownerSpec
	other.ProjectID = strings.Repeat("6", 24)
	remove, err := GuestRootRemoveCommand(other)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-c", remove).CombinedOutput(); err == nil || !strings.Contains(string(output), "different project") {
		t.Fatalf("removal error = %v, output = %q", err, output)
	}
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "keep" {
		t.Fatalf("different-owner removal modified tree: contents=%q err=%v", contents, err)
	}
}

func TestGuestRootRemoveDeletesOnlyOwnedTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	spec := SessionSpec{ProjectID: strings.Repeat("7", 24), GuestRoot: "~/workspaces/old-project"}
	prepare, err := GuestRootCommand(spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-c", prepare).CombinedOutput(); err != nil {
		t.Fatalf("preparing old root: %v: %s", err, output)
	}
	root := filepath.Join(home, "workspaces", "old-project")
	if err := os.WriteFile(filepath.Join(root, "owned-sentinel"), []byte("remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	neighbor := filepath.Join(home, "workspaces", "old-project-neighbor", "sentinel")
	if err := os.MkdirAll(filepath.Dir(neighbor), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(neighbor, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	remove, err := GuestRootRemoveCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-c", remove).CombinedOutput(); err != nil {
		t.Fatalf("removing old root: %v: %s", err, output)
	}
	owner := filepath.Join(home, ".cloister", "guest-root-owners", "workspaces", "old-project.owner")
	for _, removed := range []string{root, owner} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Errorf("%q remains after completed removal: %v", removed, err)
		}
	}
	if contents, err := os.ReadFile(neighbor); err != nil || string(contents) != "keep" {
		t.Fatalf("owned-tree removal modified neighbour: contents=%q err=%v", contents, err)
	}
}

func TestGuestRootRemoveRetriesInterruptedClaimCleanup(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		needle       string
		replacement  string
		ownerRemains bool
		claimRemains bool
		markerExists bool
	}{
		{
			name:         "after tree deletion",
			needle:       ` && mv -- "$owner/project-id" "$removal"`,
			replacement:  `; exit 74; mv -- "$owner/project-id" "$removal"`,
			ownerRemains: true,
			claimRemains: true,
		},
		{
			name:         "after ownership moves to removal marker",
			needle:       ` && rmdir -- "$owner"`,
			replacement:  `; exit 75; rmdir -- "$owner"`,
			ownerRemains: true,
			markerExists: true,
		},
		{
			name:         "after claim directory removal",
			needle:       ` && rm -f -- "$removal"`,
			replacement:  `; exit 76; rm -f -- "$removal"`,
			markerExists: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			spec := SessionSpec{ProjectID: strings.Repeat("8", 24), GuestRoot: "~/workspaces/interrupted"}
			prepare, err := GuestRootCommand(spec, false)
			if err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("sh", "-c", prepare).CombinedOutput(); err != nil {
				t.Fatalf("preparing owned root: %v: %s", err, output)
			}
			target := filepath.Join(home, "workspaces", "interrupted")
			if err := os.WriteFile(filepath.Join(target, "sentinel"), []byte("remove"), 0o600); err != nil {
				t.Fatal(err)
			}
			remove, err := GuestRootRemoveCommand(spec)
			if err != nil {
				t.Fatal(err)
			}
			interrupted := strings.Replace(remove, testCase.needle, testCase.replacement, 1)
			if interrupted == remove {
				t.Fatalf("remove command has no interruption seam %q: %q", testCase.needle, remove)
			}
			if output, err := exec.Command("sh", "-c", interrupted).CombinedOutput(); err == nil {
				t.Fatalf("interrupted removal exited successfully: %s", output)
			}

			owner := filepath.Join(home, ".cloister", "guest-root-owners", "workspaces", "interrupted.owner")
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("interruption occurred before tree deletion: %v", err)
			}
			if _, err := os.Stat(owner); testCase.ownerRemains != (err == nil) {
				t.Fatalf("owner existence = %v, want %v (err=%v)", err == nil, testCase.ownerRemains, err)
			}
			claim, claimErr := os.ReadFile(filepath.Join(owner, "project-id"))
			if testCase.claimRemains != (claimErr == nil) {
				t.Fatalf("claim existence = %v, want %v (err=%v)", claimErr == nil, testCase.claimRemains, claimErr)
			}
			if claimErr == nil && strings.TrimSpace(string(claim)) != spec.ProjectID {
				t.Fatalf("claim project ID = %q, want %q", claim, spec.ProjectID)
			}
			marker, markerErr := os.ReadFile(owner + ".removing")
			if testCase.markerExists != (markerErr == nil) {
				t.Fatalf("marker existence = %v, want %v (err=%v)", markerErr == nil, testCase.markerExists, markerErr)
			}
			if markerErr == nil && strings.TrimSpace(string(marker)) != spec.ProjectID {
				t.Fatalf("marker project ID = %q, want %q", marker, spec.ProjectID)
			}

			if output, err := exec.Command("sh", "-c", remove).CombinedOutput(); err != nil {
				t.Fatalf("retrying interrupted removal: %v: %s", err, output)
			}
			for _, removed := range []string{target, owner, owner + ".removing"} {
				if _, err := os.Stat(removed); !os.IsNotExist(err) {
					t.Errorf("%q remains after retry: %v", removed, err)
				}
			}
			if output, err := exec.Command("sh", "-c", remove).CombinedOutput(); err != nil {
				t.Fatalf("retrying completed removal: %v: %s", err, output)
			}
		})
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

func TestMutagenRecreatesSessionWithChangedIgnorePolicy(t *testing.T) {
	root := t.TempDir()
	ignorePath := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	var log bytes.Buffer
	m := &Mutagen{
		Binary:  "mutagen",
		Runner:  runner,
		DataDir: filepath.Join(t.TempDir(), "data"),
		SSHDir:  filepath.Join(t.TempDir(), "ssh"),
		SSHPath: "/usr/bin/ssh",
		SCPPath: "/usr/bin/scp",
		Log:     &log,
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
	runner.StatusOutput = liveSessionOutput(spec.Name, spec.HostRoot, spec.GuestRoot)
	if err := m.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	wantOperations := []string{"sync list", "sync list", "sync terminate", "sync create"}
	if len(runner.Calls) != 2+len(wantOperations) {
		t.Fatalf("calls = %#v, want initial create plus %v", runner.Calls, wantOperations)
	}
	for i, want := range wantOperations {
		got := strings.Join(runner.Calls[i+2].Args[:2], " ")
		if got != want {
			t.Fatalf("recovery call %d = %q, want %q", i, got, want)
		}
	}
	if !strings.Contains(log.String(), "terminating the stale session") || !strings.Contains(log.String(), "fresh synchronization history") {
		t.Fatalf("recovery log = %q", log.String())
	}

	other := spec
	other.Name = "cloister-other-" + spec.ProjectID
	if m.policyPath(spec) == m.policyPath(other) {
		t.Fatal("policy fingerprints are not isolated by profile-project session")
	}
}

func TestMutagenPolicyRecoveryFailsClosedWhenTerminationFails(t *testing.T) {
	root := t.TempDir()
	ignorePath := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	m := &Mutagen{
		Binary: "mutagen", Runner: runner, DataDir: filepath.Join(t.TempDir(), "data"),
		SSHDir: filepath.Join(t.TempDir(), "ssh"), SSHPath: "/usr/bin/ssh", SCPPath: "/usr/bin/scp",
		Log: &bytes.Buffer{},
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
	runner.StatusOutput = liveSessionOutput(spec.Name, spec.HostRoot, spec.GuestRoot)
	runner.CommandErrors = map[string]error{"sync terminate": runnerExitError(1)}
	runner.CommandOutput = map[string]string{"sync terminate": "termination denied"}

	err = m.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "refusing to recreate") || !strings.Contains(err.Error(), "termination denied") {
		t.Fatalf("Create() error = %v", err)
	}
	if len(runner.Calls) != 5 {
		t.Fatalf("calls = %#v, want initial status/create then status/status/terminate", runner.Calls)
	}
	for _, call := range runner.Calls[2:] {
		if len(call.Args) >= 2 && call.Args[0] == "sync" && call.Args[1] == "create" {
			t.Fatalf("created replacement after failed termination: %#v", runner.Calls)
		}
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

func TestMutagenVerifyGuestRootAvailableRefusesLiveOwner(t *testing.T) {
	owner := SessionSpec{
		Profile: "work", ProjectID: strings.Repeat("1", 24),
		Name: "cloister-work-" + strings.Repeat("1", 24), HostRoot: "/host/owner", GuestRoot: "~/workspaces/shared",
	}
	requested := SessionSpec{
		Profile: "work", ProjectID: strings.Repeat("2", 24),
		Name: "cloister-work-" + strings.Repeat("2", 24), HostRoot: "/host/requested", GuestRoot: owner.GuestRoot,
	}
	runner := &fakeRunner{StatusOutput: liveSessionOutput(owner.Name, owner.HostRoot, owner.GuestRoot)}
	m := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: t.TempDir()}

	err := m.VerifyGuestRootAvailable(context.Background(), requested, "")
	if err == nil || !strings.Contains(err.Error(), owner.Name) {
		t.Fatalf("VerifyGuestRootAvailable() error = %v", err)
	}
}

func TestMutagenOldGuestRootAllowsOnlyMigratingSession(t *testing.T) {
	root := "~/workspaces/shared-old"
	migrating := SessionSpec{
		Profile: "work", ProjectID: strings.Repeat("1", 24),
		Name: "cloister-work-" + strings.Repeat("1", 24), HostRoot: "/host/one", GuestRoot: root,
	}
	other := SessionSpec{
		Profile: "work", ProjectID: strings.Repeat("2", 24),
		Name: "cloister-work-" + strings.Repeat("2", 24), HostRoot: "/host/two", GuestRoot: root,
	}
	runner := &fakeRunner{StatusOutput: liveSessionOutput(migrating.Name, migrating.HostRoot, root) + liveSessionOutput(other.Name, other.HostRoot, root)}
	m := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: t.TempDir()}

	err := m.VerifyGuestRootAvailable(context.Background(), migrating, migrating.Name)
	if err == nil || !strings.Contains(err.Error(), other.Name) {
		t.Fatalf("VerifyGuestRootAvailable() error = %v", err)
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

func TestParseMutagenStatusAcceptsRealPausedSessionOutput(t *testing.T) {
	status, err := parseMutagenStatus([]byte(realMutagenPausedSessionOutput))
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StatePaused {
		t.Fatalf("state = %q, want paused", status.State)
	}
	if status.ConflictCount != 0 || len(status.Problems) != 0 {
		t.Fatalf("paused endpoint disconnection recorded as a problem: %#v", status)
	}
	if err := status.Clean(); err != nil {
		t.Fatalf("Clean() error = %v, want clean paused session", err)
	}
}

// A healthy Mutagen session reports scan, stage, apply, and save statuses
// after a blocking flush returns. Treating those as problems incorrectly
// rejected a completed flush barrier.
func TestParseMutagenStatusTreatsDocumentedProgressAsActive(t *testing.T) {
	for _, description := range []string{
		"Watching for changes",
		"Scanning files",
		"Reconciling changes",
		"Staging files on alpha",
		"Staging files on beta",
		"Applying changes",
		"Saving archive",
	} {
		t.Run(description, func(t *testing.T) {
			status, err := parseMutagenStatus([]byte(activeMutagenSessionOutput(description)))
			if err != nil {
				t.Fatal(err)
			}
			if status.State != StateActive || len(status.Problems) != 0 || status.ConflictCount != 0 {
				t.Fatalf("status = %#v, want active progress without problems", status)
			}
			if status.Description != description {
				t.Fatalf("description = %q, want %q", status.Description, description)
			}
			if err := status.Clean(); err != nil {
				t.Fatalf("Clean() error = %v, want a clean progressing session", err)
			}
		})
	}
}

func TestParseMutagenStatusFailsClosedForNonProgressStatus(t *testing.T) {
	for _, description := range []string{
		"Disconnected",
		"Halted due to one-sided root emptying",
		"Halted due to root deletion",
		"Halted due to root type change",
		"Connecting to alpha",
		"Connecting to beta",
		"Waiting 5 seconds for rescan",
		"Unknown",
	} {
		t.Run(description, func(t *testing.T) {
			status, err := parseMutagenStatus([]byte(activeMutagenSessionOutput(description)))
			if err != nil {
				t.Fatal(err)
			}
			if status.State != StateProblem || len(status.Problems) == 0 {
				t.Fatalf("status = %#v, want a non-progress status to fail closed", status)
			}
			if !strings.Contains(strings.Join(status.Problems, "; "), description) {
				t.Fatalf("problems = %#v, want the status description", status.Problems)
			}
			if err := status.Clean(); err == nil {
				t.Fatalf("Clean() = nil for non-progress status %q", description)
			}
		})
	}
}

func TestParseMutagenStatusFailsClosedForRescanWaitWithLastError(t *testing.T) {
	const lastError = "beta scan error: invalid symbolic link (vendor/bin/tool): target is absolute"
	output := "--------------------------------------------------------------------------------\n" +
		"Name: cloister-test-profile-0123456789abcdef\n" +
		"Identifier: sync_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789\n" +
		"Alpha:\n\tURL: /Users/example/project\n\tConnected: Yes\n" +
		"Beta:\n\tURL: ssh://cloister-sync-0123456789abcdef/~/workspaces/project-0123456789ab\n\tConnected: Yes\n" +
		"Last error: " + lastError + "\n" +
		"Status: Waiting 5 seconds for rescan\n" +
		"--------------------------------------------------------------------------------\n"
	status, err := parseMutagenStatus([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateProblem {
		t.Fatalf("state = %q, want problem", status.State)
	}
	if !strings.Contains(strings.Join(status.Problems, "; "), lastError) {
		t.Fatalf("problems = %#v, want the Last error text", status.Problems)
	}
	if err := status.Clean(); err == nil {
		t.Fatal("Clean() = nil for a scan failure waiting to rescan")
	} else if !strings.Contains(err.Error(), lastError) {
		t.Fatalf("Clean() error = %v, want the real scan error", err)
	}
}

func TestParseMutagenStatusKeepsRealProblemsDuringProgress(t *testing.T) {
	tests := []struct {
		name          string
		output        string
		wantProblem   string
		wantConflicts int
	}{
		{
			name: "disconnected endpoint",
			output: "Name: cloister-test-profile-0123456789abcdef\n" +
				"Alpha:\n\tConnected: Yes\nBeta:\n\tConnected: No\n" +
				"Status: Scanning files\n",
			wantProblem: "Connected: No",
		},
		{
			name: "last error",
			output: "Name: cloister-test-profile-0123456789abcdef\n" +
				"Alpha:\n\tConnected: Yes\nBeta:\n\tConnected: Yes\n" +
				"Last error: unable to stage files on beta\n" +
				"Status: Staging files on beta\n",
			wantProblem: "unable to stage files on beta",
		},
		{
			name: "conflicts",
			output: "Name: cloister-test-profile-0123456789abcdef\n" +
				"Alpha:\n\tConnected: Yes\nBeta:\n\tConnected: Yes\n" +
				"Conflicts: 2\nStatus: Reconciling changes\n",
			wantConflicts: 2,
		},
		{
			name: "scan problems",
			output: "Name: cloister-test-profile-0123456789abcdef\n" +
				"Alpha:\n\tConnected: Yes\n\tScan problems:\n\t\tsource.go: unable to read file\n" +
				"Beta:\n\tConnected: Yes\n" +
				"Status: Scanning files\n",
			wantProblem: "Scan problems:",
		},
		{
			name: "transition problems",
			output: "Name: cloister-test-profile-0123456789abcdef\n" +
				"Alpha:\n\tConnected: Yes\n" +
				"Beta:\n\tConnected: Yes\n\tTransition problems:\n\t\tgenerated/output: unable to create file\n" +
				"Status: Applying changes\n",
			wantProblem: "Transition problems:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, err := parseMutagenStatus([]byte(test.output))
			if err != nil {
				t.Fatal(err)
			}
			if err := status.Clean(); err == nil {
				t.Fatalf("Clean() = nil for a progressing session with real problems: %#v", status)
			}
			if status.ConflictCount != test.wantConflicts {
				t.Fatalf("conflicts = %d, want %d", status.ConflictCount, test.wantConflicts)
			}
			if test.wantProblem == "" {
				return
			}
			if status.State != StateProblem {
				t.Fatalf("state = %q, want problem", status.State)
			}
			if !strings.Contains(strings.Join(status.Problems, "; "), test.wantProblem) {
				t.Fatalf("problems = %#v, want one reporting %q", status.Problems, test.wantProblem)
			}
		})
	}
}

func TestParseMutagenStatusKeepsActiveEndpointDisconnectionProblematic(t *testing.T) {
	output := "Name: cloister-local-dev-222222222222222222222222\n" +
		"Alpha:\n\tConnected: Yes\nBeta:\n\tConnected: No\n" +
		"Conflicts: 0\nStatus: Watching for changes\n"
	status, err := parseMutagenStatus([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateProblem || len(status.Problems) == 0 {
		t.Fatalf("status = %#v, want active disconnection problem", status)
	}
	if err := status.Clean(); err == nil {
		t.Fatal("Clean() = nil for a disconnected active session")
	}
}

func TestParseMutagenStatusFailsClosedForPausedSessionWithRealProblems(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name: "last error",
			output: "Name: cloister-local-dev-222222222222222222222222\n" +
				"Alpha:\n\tConnected: No\nBeta:\n\tConnected: No\n" +
				"Last error: transport failed\nStatus: [Paused]\n",
		},
		{
			name: "endpoint problems",
			output: "Name: cloister-local-dev-222222222222222222222222\n" +
				"Alpha:\n\tConnected: No\nBeta:\n\tConnected: No\n" +
				"Beta problems: permission denied\nStatus: [Paused]\n",
		},
		{
			name: "conflicts",
			output: "Name: cloister-local-dev-222222222222222222222222\n" +
				"Alpha:\n\tConnected: No\nBeta:\n\tConnected: No\n" +
				"Conflicts: 2\nStatus: [Paused]\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, err := parseMutagenStatus([]byte(test.output))
			if err != nil {
				t.Fatal(err)
			}
			if err := status.Clean(); err == nil {
				t.Fatalf("Clean() = nil for paused session with real problems: %#v", status)
			}
		})
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
	if len(report.Material) != 1 || report.Material[0].Attribute != "com.example.material" {
		t.Fatalf("material findings = %v", report.Material)
	}
	// The hardlinked pair lives under a mandatorily ignored tree, so it is
	// neither a finding nor a refusal.
	if report.Material[0].Files != 1 {
		t.Fatalf("material files = %d, want only the included file", report.Material[0].Files)
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

func TestWorkspaceGuestRootMirrorsRelativePath(t *testing.T) {
	for _, testCase := range []struct {
		relative string
		want     string
	}{
		{"apps/AWSCrossReference", "~/workspaces/apps/AWSCrossReference"},
		{"worktrees/meyer-integration/Service", "~/workspaces/worktrees/meyer-integration/Service"},
		{"apps/api service", "~/workspaces/apps/api-service"},
		{"tools/rockauto.scraper", "~/workspaces/tools/rockauto.scraper"},
	} {
		got, err := WorkspaceGuestRoot(testCase.relative)
		if err != nil {
			t.Fatalf("WorkspaceGuestRoot(%q) error = %v", testCase.relative, err)
		}
		if got != testCase.want {
			t.Errorf("WorkspaceGuestRoot(%q) = %q, want %q", testCase.relative, got, testCase.want)
		}
	}
}

func TestWorkspaceGuestRootRejectsUnusableSegments(t *testing.T) {
	for _, relative := range []string{"apps/..", "../escape", "apps//api", "apps/.", "apps/+"} {
		if got, err := WorkspaceGuestRoot(relative); err == nil {
			t.Errorf("WorkspaceGuestRoot(%q) = %q, want an error", relative, got)
		}
	}
}

func TestGuestRootCommandsRejectTraversal(t *testing.T) {
	// "." and "/" are both inside the permitted character set, so a dot-only
	// segment is the one way a guest root can satisfy the character check and
	// still escape ~/workspaces. GuestRootResetCommand interpolates the result
	// into an rm -rf, so this is the boundary that matters most.
	for _, guestRoot := range []string{
		"~/workspaces/../../etc",
		"~/workspaces/project/../../..",
		"~/workspaces/./project",
		"~/workspaces//project",
	} {
		spec := SessionSpec{ProjectID: strings.Repeat("1", 24), GuestRoot: guestRoot}
		if _, err := GuestRootCommand(spec, false); err == nil {
			t.Errorf("GuestRootCommand(%q) error = nil, want a refusal", guestRoot)
		}
		if _, err := GuestRootResetCommand(spec); err == nil {
			t.Errorf("GuestRootResetCommand(%q) error = nil, want a refusal", guestRoot)
		}
		if _, err := GuestShellCommand(spec); err == nil {
			t.Errorf("GuestShellCommand(%q) error = nil, want a refusal", guestRoot)
		}
	}
}

// TestPrepareSSHDisablesConnectionMultiplexing pins the opt-out that keeps a
// large workspace collection from exhausting the shared control socket's
// session slots. Each per-project fragment includes the hypervisor's generated
// SSH config, which turns multiplexing on; without an override ahead of those
// includes every session past the guest's MaxSessions limit fails its
// multiplexed session request and falls back to an unmultiplexed connection,
// printing a pair of diagnostics for each attempt.
func TestPrepareSSHDisablesConnectionMultiplexing(t *testing.T) {
	dir := t.TempDir()
	m := &Mutagen{
		SSHDir:  filepath.Join(dir, "mutagen-ssh"),
		SSHPath: "/usr/bin/ssh",
		SCPPath: "/usr/bin/scp",
	}
	hypervisorConfig := filepath.Join(dir, "ssh.config")
	if err := os.WriteFile(hypervisorConfig, []byte("Host vm\n  ControlMaster auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.prepareSSH(SessionSpec{
		Profile:   "work",
		ProjectID: "0123456789abcdef0123",
		SSH:       vm.SSHAccess{HostAlias: "vm", ConfigFile: hypervisorConfig},
	}); err != nil {
		t.Fatalf("prepareSSH() error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(m.SSHDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(raw)
	for _, option := range []string{"ControlMaster no", "ControlPath none"} {
		if !strings.Contains(config, option) {
			t.Errorf("Mutagen SSH config missing %q; got:\n%s", option, config)
		}
	}
	// ssh_config takes the first value it sees for an option, so the opt-out
	// only wins if it precedes the includes that turn multiplexing on.
	if optOut, include := strings.Index(config, "ControlMaster no"), strings.Index(config, "Include "); optOut < 0 || include < 0 || optOut > include {
		t.Errorf("multiplexing opt-out must precede the includes; got:\n%s", config)
	}
}

// A changed endpoint requires filesystem cleanup around session termination,
// so the engine adapter must not recreate it outside the lifecycle coordinator.
func TestMutagenRefusesUncoordinatedGuestRootMove(t *testing.T) {
	root := t.TempDir()
	var log bytes.Buffer
	runner := &fakeRunner{}
	m := &Mutagen{
		Binary: "mutagen", Runner: runner, DataDir: filepath.Join(t.TempDir(), "data"),
		SSHDir: filepath.Join(t.TempDir(), "ssh"), SSHPath: "/usr/bin/ssh", SCPPath: "/usr/bin/scp",
		Log: &log,
	}
	spec, err := BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	// The live session still points at the previous flat guest path while the
	// specification now asks for the mirrored one.
	runner.StatusOutput = liveSessionOutput(spec.Name, spec.HostRoot, "~/workspaces/project-0123456789ab")
	spec.GuestRoot = "~/workspaces/worktrees/some-set/Project"
	runner.Calls = nil
	err = m.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "must be coordinated") {
		t.Fatalf("Create() error = %v", err)
	}

	operations := make([]string, 0, len(runner.Calls))
	for _, call := range runner.Calls {
		if len(call.Args) >= 2 {
			operations = append(operations, strings.Join(call.Args[:2], " "))
		}
	}
	want := []string{"sync list"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
	if log.Len() != 0 {
		t.Errorf("uncoordinated migration logged a destructive action: %q", log.String())
	}
}

// TestMutagenRefusesSessionWithUnreadableGuestEndpoint verifies the check fails
// closed. Resuming a session whose destination could not be read would risk
// synchronizing to a path nobody verified.
func TestMutagenRefusesSessionWithUnreadableGuestEndpoint(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{}
	m := &Mutagen{
		Binary: "mutagen", Runner: runner, DataDir: filepath.Join(t.TempDir(), "data"),
		SSHDir: filepath.Join(t.TempDir(), "ssh"), SSHPath: "/usr/bin/ssh", SCPPath: "/usr/bin/scp",
		Log: &bytes.Buffer{},
	}
	spec, err := BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	runner.StatusOutput = "Name: " + spec.Name + "\nStatus: Watching for changes\n"
	err = m.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "no guest endpoint path") {
		t.Fatalf("Create() error = %v, want a refusal naming the unreadable endpoint", err)
	}
}

func TestMutagenRefusesSessionOwnedByDifferentHostProject(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{}
	m := &Mutagen{
		Binary: "mutagen", Runner: runner, DataDir: filepath.Join(t.TempDir(), "data"),
		SSHDir: filepath.Join(t.TempDir(), "ssh"), SSHPath: "/usr/bin/ssh", SCPPath: "/usr/bin/scp",
		Log: &bytes.Buffer{},
	}
	spec, err := BuildSessionSpec("work", root, vm.SSHAccess{Host: "vm.local", User: "guest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner.StatusOutput = liveSessionOutput(spec.Name, "/host/different-project", spec.GuestRoot)

	err = m.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "belongs to host project") {
		t.Fatalf("Create() error = %v", err)
	}
	if len(runner.Calls) != 1 {
		t.Fatalf("different host project triggered destructive calls: %#v", runner.Calls)
	}
}

func TestBetaGuestPathHandlesBothEndpointForms(t *testing.T) {
	for _, testCase := range []struct{ url, want string }{
		{"vm.local:~/workspaces/apps/Api", "~/workspaces/apps/Api"},
		{"ssh://cloister-sync-0123/~/workspaces/apps/Api", "~/workspaces/apps/Api"},
		{"ssh://cloister-sync-0123/srv/checkout", "/srv/checkout"},
		{"/Users/example/project", "/Users/example/project"},
	} {
		if got := betaGuestPath(testCase.url); got != testCase.want {
			t.Errorf("betaGuestPath(%q) = %q, want %q", testCase.url, got, testCase.want)
		}
	}
}
