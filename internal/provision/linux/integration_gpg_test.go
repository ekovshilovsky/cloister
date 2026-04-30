//go:build integration_gpg
// +build integration_gpg

package linux

import (
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
func vmExec(t *testing.T, command string) (string, error) {
	t.Helper()
	out, err := exec.Command("cloister", "exec", testProfile, "--", "bash", "-c", command).CombinedOutput()
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
	// key generation. If this command succeeds, we are connected to the wrong
	// socket and the security property of this design is broken.
	spec := "Key-Type: RSA\nKey-Length: 1024\nName-Real: ShouldNotExist\n%commit\n"
	out, err := vmExec(t, "echo '"+spec+"' | gpg --batch --gen-key 2>&1")
	if err == nil && !strings.Contains(out, "Forbidden") &&
		!strings.Contains(out, "not allowed") &&
		!strings.Contains(out, "permission denied") {
		t.Fatalf("gpg --gen-key unexpectedly succeeded — VM is connected to the FULL agent socket, not the restricted extra-socket. Output:\n%s", out)
	}
}

func TestGPGForwardSurvivesVMRestart(t *testing.T) {
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
	if err := exec.Command("gpgconf", "--kill", "gpg-agent").Run(); err != nil {
		t.Fatalf("killing host gpg-agent: %v", err)
	}
	defer exec.Command("gpgconf", "--launch", "gpg-agent").Run()

	out, err := vmExec(t, "timeout 10 bash -c 'echo dead-agent | gpg --batch --clearsign' 2>&1")
	if err == nil {
		t.Errorf("expected sign to fail when host agent is dead; got success:\n%s", out)
	}
}
