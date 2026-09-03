package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cloister.io/internal/config"
	linuxprov "cloister.io/internal/provision/linux"
	macosprov "cloister.io/internal/provision/macos"
	"cloister.io/internal/vm"
	vmlume "cloister.io/internal/vm/lume"
)

func TestPrintWorkspaceCleanupWarningsNamesPreservedRealEntry(t *testing.T) {
	var out strings.Builder
	printWorkspaceCleanupWarnings(&out, linuxprov.WorkspaceCleanupReport{
		PreservedAliases: []string{"~/code"},
	})
	got := out.String()
	if !strings.Contains(got, "warning:") || !strings.Contains(got, "~/code") || !strings.Contains(got, "preserving") {
		t.Fatalf("cleanup warning = %q, want warning naming preserved ~/code", got)
	}
}

func TestPrintBashrcReplacementNoticeDisclosesOverwrite(t *testing.T) {
	var out strings.Builder
	printBashrcReplacementNotice(&out)
	got := out.String()
	if !strings.Contains(got, "~/.bashrc") || !strings.Contains(got, "differed") || !strings.Contains(got, "replaced") {
		t.Fatalf("bashrc replacement notice = %q", got)
	}
}

// guestResponder answers each guest command individually, which is what a
// repair needs: the checks and the fixes are different commands with different
// outcomes.
func guestResponder(reply func(command string) (string, error)) *vm.MockBackend {
	return &vm.MockBackend{SSHCommandFunc: func(_, command string) (string, error) {
		return reply(command)
	}}
}

// everyCheckPasses answers as a VM that is already fully configured.
func everyCheckPasses(profile string) func(string) (string, error) {
	return func(command string) (string, error) {
		switch {
		case strings.Contains(command, "/etc/sudoers.d/lume"):
			return "lume ALL=(ALL) NOPASSWD: ALL\n", nil
		case strings.Contains(command, "scutil --get LocalHostName"):
			return vmlume.Hostname(profile) + "\n", nil
		}
		return "", nil
	}
}

// A command that reports failure in its output and success in its exit status
// is worse than one that does neither, because a script reads the status and
// carries on.
func TestRepairLumeProfileReportsFailureInItsExitStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	backend := guestResponder(func(string) (string, error) {
		return "sudo: a password is required\n", errors.New("exit status 1")
	})

	err := repairLumeProfile("mac", &config.Profile{}, backend)

	if err == nil {
		t.Fatal("repair returned success with every check still failing")
	}
	if !strings.Contains(err.Error(), "mac") {
		t.Errorf("error does not name the profile: %v", err)
	}
}

// A repair that fixed everything it found is the success case, and has to stay
// distinguishable from the one above.
func TestRepairLumeProfileSucceedsWhenNothingIsStillFailing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	backend := guestResponder(everyCheckPasses("mac"))

	if err := repairLumeProfile("mac", &config.Profile{}, backend); err != nil {
		t.Fatalf("repair of an already-configured VM returned %v, want success", err)
	}
}

// Partial success is failure for the purposes of an exit status: one check left
// failing means the configuration the command promises is not in place, and a
// caller reading only the status cannot tell which half it got.
func TestRepairLumeProfileFailsWhenASingleCheckIsStillFailing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	passing := everyCheckPasses("mac")
	var stubborn string
	backend := guestResponder(func(command string) (string, error) {
		if strings.Contains(command, "path_helper") {
			stubborn = command
			return "", errors.New("exit status 1")
		}
		return passing(command)
	})

	err := repairLumeProfile("mac", &config.Profile{}, backend)

	if err == nil {
		t.Fatalf("repair returned success with one check (%q) still failing", stubborn)
	}
	if !strings.Contains(err.Error(), "SSH PATH includes paths.d") {
		t.Errorf("error does not name the check that is still failing: %v", err)
	}
}

// A check that could not be fixed has to say what the guest said. Reporting
// only that it failed leaves the reader with a second command to run before
// learning anything.
func TestRepairLumeProfileRecordsWhatTheGuestSaidAboutAFailedCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	backend := guestResponder(func(string) (string, error) {
		return "", errors.New("ssh command in mac: exit status 1\nsudo: a password is required")
	})

	got := captureStderr(t, func() {
		_ = repairLumeProfile("mac", &config.Profile{}, backend)
	})

	if !strings.Contains(got, "a password is required") {
		t.Errorf("failed check reported nothing the guest said: %q", got)
	}
}

// The console carries progress; the guest output belongs in the run log. The
// Colima path already works this way and the Lume path has to match, or
// --verbose and the run log mean different things per backend.
func TestRepairLumeProfileKeepsGuestOutputOffTheConsole(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	backend := guestResponder(func(command string) (string, error) {
		if strings.Contains(command, "/etc/sudoers.d/lume") {
			return "lume ALL=(ALL) NOPASSWD: ALL\nreams of guest chatter\n", nil
		}
		return everyCheckPasses("mac")(command)
	})

	got := captureStdout(t, func() {
		_ = repairLumeProfile("mac", &config.Profile{}, backend)
	})

	if strings.Contains(got, "reams of guest chatter") {
		t.Errorf("guest output reached the console: %q", got)
	}
	if !strings.Contains(got, "passwordless sudo") {
		t.Errorf("console does not name the checks it ran: %q", got)
	}
}

// The Colima repair path has four steps that report a problem and carry on --
// a profile whose GPG setup failed still has a usable VM. Ending such a run
// with "all steps passed" is the same untruth as a success exit status over a
// failed one, in the same command.
func TestRepairSummaryDoesNotClaimEveryStepPassedWhenOneWarned(t *testing.T) {
	got := repairSummary("work", []string{"Git configuration", "GitHub CLI authentication"})

	if strings.Contains(got, "all steps passed") {
		t.Errorf("summary claims every step passed after two warned: %q", got)
	}
	for _, name := range []string{"Git configuration", "GitHub CLI authentication"} {
		if !strings.Contains(got, name) {
			t.Errorf("summary does not name the step that warned (%q): %q", name, got)
		}
	}
}

// The clean run still has to read as a clean run.
func TestRepairSummarySaysSoWhenNothingWarned(t *testing.T) {
	got := repairSummary("work", nil)

	if !strings.Contains(got, "all steps passed") {
		t.Errorf("summary of a clean repair = %q, want it to say every step passed", got)
	}
}

// The session is what knows which steps warned, since it is what they were
// reported through.
func TestSessionRemembersTheStepsThatWarned(t *testing.T) {
	session := newProvisionSession(io.Discard, io.Discard, false, false)
	defer session.Close()

	session.Step("Base tools").Done()
	session.Step("Git configuration").Warn("git config: host identity missing")
	session.Step("Configuration").Done()
	session.Step("GitHub CLI authentication").Warn("gh auth: not authenticated")

	want := []string{"Git configuration", "GitHub CLI authentication"}
	if got := session.Warned(); !reflect.DeepEqual(got, want) {
		t.Errorf("Warned() = %v, want %v", got, want)
	}
}

// The rebooted VM is the state repair promises to leave behind. A transient
// first-pass failure must not turn a clean post-reboot verification into a
// failing command.
func TestRepairChecksReportOnlyTheLatestVerificationPass(t *testing.T) {
	session := newProvisionSession(io.Discard, io.Discard, false, false)
	defer session.Close()

	checkCalls := 0
	checks := &repairChecks{session: session, guest: func(command string) (string, error) {
		if command == "check" {
			checkCalls++
			if checkCalls > 2 {
				return "", nil
			}
		}
		return "", errors.New("transient failure")
	}}
	steps := []macosprov.Step{{Name: "transient setting", Check: "check", Install: "install"}}

	runRepairPass(checks, steps)
	runRepairPass(checks, steps)

	if err := checks.report("the base image"); err != nil {
		t.Fatalf("clean final verification returned failure: %v", err)
	}
}

// A condition that fails on both sides of the reboot is one final failure,
// not two historical observations of it.
func TestRepairChecksDoNotDoubleCountPersistentFailures(t *testing.T) {
	session := newProvisionSession(io.Discard, io.Discard, false, false)
	defer session.Close()
	checks := &repairChecks{session: session, guest: func(string) (string, error) {
		return "", errors.New("persistent failure")
	}}
	steps := []macosprov.Step{{Name: "persistent setting", Check: "check", Install: "install"}}

	runRepairPass(checks, steps)
	runRepairPass(checks, steps)
	err := checks.report("the base image")

	if err == nil {
		t.Fatal("persistent final failure returned success")
	}
	if !strings.Contains(err.Error(), "1 check") {
		t.Errorf("persistent failure count = %q, want one check", err)
	}
	if got := strings.Count(err.Error(), "persistent setting"); got != 1 {
		t.Errorf("persistent condition named %d times in %q, want once", got, err)
	}
}

// A reboot can undo a setting and require the same repair again. The work was
// performed twice, but the summary reports distinct conditions rather than a
// history of attempts.
func TestRepairChecksReportRepairedConditionOnceAcrossPasses(t *testing.T) {
	session := newProvisionSession(io.Discard, io.Discard, false, false)
	defer session.Close()
	checkCalls := 0
	checks := &repairChecks{session: session, guest: func(command string) (string, error) {
		if command == "check" {
			checkCalls++
			if checkCalls%2 == 1 {
				return "", errors.New("setting absent")
			}
		}
		return "", nil
	}}
	steps := []macosprov.Step{{Name: "reboot-sensitive setting", Check: "check", Install: "install"}}

	runRepairPass(checks, steps)
	runRepairPass(checks, steps)
	output := captureStdout(t, func() {
		if err := checks.report("the base image"); err != nil {
			t.Fatalf("repaired condition returned failure: %v", err)
		}
	})

	if !strings.Contains(output, "1 check repaired") {
		t.Errorf("repair summary = %q, want one repaired check", output)
	}
	if got := strings.Count(output, "reboot-sensitive setting"); got != 1 {
		t.Errorf("repaired condition named %d times in %q, want once", got, output)
	}
}

// Unreachable SSH makes every repair check fail in the same way. Even then,
// the console should remain a summary: one diagnostic tail, one log location,
// and no connection-target tracing unless verbose output was requested.
func TestFullyFailingLumeRepairKeepsConsoleReadable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installUnavailableLumeSSH(t)

	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			fmt.Printf("Repairing profile %q (backend: lume)...\n", "mac")
			err := repairLumeProfile("mac", &config.Profile{}, &vmlume.Backend{})
			if err == nil {
				t.Fatal("fully failing repair returned success")
			}
			fmt.Fprintln(os.Stderr, err)
		})
	})
	console := stdout + stderr

	if got := strings.Count(console, "SSH target for"); got != 0 {
		t.Errorf("console contains %d SSH target diagnostics, want none", got)
	}
	if got := strings.Count(console, "last "); got != 1 {
		t.Errorf("console contains %d repeated failure tails, want one", got)
	}
	if got := strings.Count(console, filepath.Join(home, ".cloister", "logs")); got != 1 {
		t.Errorf("console contains the run-log directory %d times, want once", got)
	}

	lines := strings.Count(strings.TrimSuffix(console, "\n"), "\n") + 1
	t.Logf("fully failing Lume repair emitted %d console lines", lines)
}

func installUnavailableLumeSSH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	lumeStub := "#!/bin/sh\nprintf '%s' '[{\"ipAddress\":\"127.0.0.1\"}]'\n"
	if err := os.WriteFile(filepath.Join(dir, "lume"), []byte(lumeStub), 0o700); err != nil {
		t.Fatal(err)
	}
	sshStub := "#!/bin/sh\nprintf 'ssh: connect to host 127.0.0.1 port 22: Connection refused\\n' >&2\nexit 255\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(sshStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}
