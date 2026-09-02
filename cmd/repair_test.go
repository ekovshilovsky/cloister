package cmd

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"cloister.io/internal/config"
	"cloister.io/internal/vm"
	vmlume "cloister.io/internal/vm/lume"
)

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
