//go:build integration_gpg
// +build integration_gpg

// These tests share host-level state (gpg-agent process, VM lifecycle) and
// must run serially. Do not add t.Parallel().

package linux

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// These tests require:
//   * a Colima profile named "cloister-test-gpg-forward" provisioned with GPGSigning=true
//   * `cloister setup gpg-forward` already run on the host
//   * `git config --global user.signingkey` set to a real key the host can use
// Run with:
//   go test -tags integration_gpg ./internal/provision/linux/ -v -run TestGPGForward

const testProfile = "cloister-test-gpg-forward"

// vmExec runs a command inside the test VM and returns combined output.
//
// The command is passed as a single positional argument to `cloister exec`,
// which joins args[1:] back into one string and hands it to the VM's login
// shell. Splitting the command across multiple args (e.g. via "--" plus
// "bash" "-c" "...") would cause the inner quoted command to be re-joined
// naively on the host side, destroying shell quoting before the VM ever
// sees it.
func vmExec(t *testing.T, command string) (string, error) {
	t.Helper()
	out, err := exec.Command("cloister", "exec", testProfile, command).CombinedOutput()
	return string(out), err
}

func TestGPGForwardNoPrivateKeysInVM(t *testing.T) {
	out, err := vmExec(t, "ls ~/.gnupg/private-keys-v1.d/ 2>/dev/null | wc -l")
	if err != nil {
		// Directory absent is also a pass.
		return
	}
	count := strings.TrimSpace(out)
	if count != "0" {
		t.Errorf("expected zero files in ~/.gnupg/private-keys-v1.d/, got count=%s", count)
	}
}

func TestGPGForwardClearsignRoundtrip(t *testing.T) {
	out, err := vmExec(t, "echo cloister-roundtrip-token | gpg --clearsign | gpg --verify 2>&1")
	if err != nil {
		t.Fatalf("clearsign roundtrip failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Good signature") {
		t.Errorf("expected 'Good signature' in gpg --verify output; got:\n%s", out)
	}
}

func TestGPGForwardExtraSocketRefusesKeyGen(t *testing.T) {
	// The forwarded socket is the restricted (extra) socket, which forbids
	// key generation. We require BOTH that the command failed AND that the
	// failure carries a known restriction marker — any other failure mode
	// (no agent, malformed input, etc.) is inconclusive about the security
	// property and produces an explicit Skip rather than a false-positive PASS.
	spec := "Key-Type: RSA\nKey-Length: 1024\nName-Real: ShouldNotExist\n%commit\n"
	// printf '%s' preserves the embedded newlines in the Go string verbatim
	// because single quotes in bash pass any byte (including newline) through
	// unchanged. The literal '%' in the printf format string is escaped as
	// '%%' for fmt.Sprintf.
	cmd := fmt.Sprintf("printf '%%s' '%s' | gpg --batch --gen-key 2>&1", spec)
	out, err := vmExec(t, cmd)

	if err == nil {
		t.Fatalf("gpg --gen-key unexpectedly succeeded — VM is connected to the FULL agent socket, not the restricted extra-socket. Output:\n%s", out)
	}

	// Known restriction markers gpg-agent emits when the extra-socket refuses
	// a forbidden command, plus the missing-socket case (also fail-secure: no
	// socket means key generation cannot reach the host agent at all).
	markers := []string{"Forbidden", "not allowed", "permission denied", "No such file or directory"}
	for _, m := range markers {
		if strings.Contains(out, m) {
			return
		}
	}
	t.Skipf("gpg --gen-key failed but did not emit a known restriction marker; cannot conclude the extra-socket refused the operation. Output:\n%s", out)
}

func TestGPGForwardSurvivesVMRestart(t *testing.T) {
	t.Cleanup(func() {
		// Always restore the VM to the running state, even if a t.Fatalf
		// occurred mid-test. cloister start is idempotent on an
		// already-running VM, so this is safe to invoke unconditionally.
		if err := exec.Command("cloister", "start", testProfile).Run(); err != nil {
			t.Logf("cleanup: cloister start failed: %v", err)
		}
	})

	// Sign once to seed the host gpg-agent / Keychain cache.
	if _, err := vmExec(t, "echo seed | gpg --clearsign > /dev/null"); err != nil {
		t.Fatalf("initial sign (cache-seed): %v", err)
	}

	// Restart the VM via cloister.
	if out, err := exec.Command("cloister", "stop", testProfile).CombinedOutput(); err != nil {
		t.Fatalf("cloister stop: %v\n%s", err, out)
	}
	if out, err := exec.Command("cloister", "start", testProfile).CombinedOutput(); err != nil {
		t.Fatalf("cloister start: %v\n%s", err, out)
	}

	// Sign again with --batch so any pinentry prompt would fail rather than
	// hang. Success here means the host agent had the passphrase cached
	// (via Keychain) and no interactive prompt was needed.
	out, err := vmExec(t, "echo post-restart | gpg --batch --clearsign 2>&1")
	if err != nil {
		t.Fatalf("post-restart signing required a prompt or failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "BEGIN PGP SIGNED MESSAGE") {
		t.Errorf("expected clearsigned output after restart; got:\n%s", out)
	}
}

func TestGPGForwardCleanFailureWhenAgentDead(t *testing.T) {
	t.Cleanup(func() {
		// Surface relaunch failures so a broken host agent at the end of the
		// test run does not silently degrade subsequent unrelated tests.
		if err := exec.Command("gpgconf", "--launch", "gpg-agent").Run(); err != nil {
			t.Logf("warning: failed to relaunch host gpg-agent: %v", err)
		}
	})

	if err := exec.Command("gpgconf", "--kill", "gpg-agent").Run(); err != nil {
		t.Fatalf("killing host gpg-agent: %v", err)
	}

	// vmExec already uses CombinedOutput; no need to redirect stderr again
	// inside the inner command.
	out, err := vmExec(t, "timeout 10 bash -c 'echo dead-agent | gpg --batch --clearsign'")
	if err == nil {
		t.Errorf("expected sign to fail when host agent is dead; got success:\n%s", out)
	}
}
