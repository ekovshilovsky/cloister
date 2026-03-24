package lume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestGenerateKey verifies that GenerateKey produces a valid Ed25519 keypair,
// writes the private key with 0600 permissions, and returns a parseable
// public key in authorized_keys format.
func TestGenerateKey(t *testing.T) {
	dir := t.TempDir()
	profile := "testprofile"

	// Override KeyDir to write into the temp directory so the test does not
	// touch the real ~/.cloister/keys path.
	origKeyDir := overrideKeyDir(dir)
	defer restoreKeyDir(origKeyDir)

	privPath, pubKey, err := GenerateKey(profile)
	if err != nil {
		t.Fatalf("GenerateKey returned unexpected error: %v", err)
	}

	// Private key file must exist.
	if _, err := os.Stat(privPath); err != nil {
		t.Fatalf("private key file does not exist at %s: %v", privPath, err)
	}

	// Private key file must be mode 0600 (owner read/write only).
	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("private key file mode = %04o, want 0600", got)
	}

	// Public key string must be non-empty.
	if pubKey == "" {
		t.Fatal("GenerateKey returned an empty public key string")
	}

	// Public key must begin with the Ed25519 key type token.
	if !strings.HasPrefix(pubKey, "ssh-ed25519") {
		t.Errorf("public key does not start with ssh-ed25519, got: %q", pubKey[:min(len(pubKey), 30)])
	}

	// Public key must be parseable by the SSH library, confirming it is a
	// syntactically valid authorized_keys entry.
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubKey)); err != nil {
		t.Errorf("public key failed to parse as authorized key: %v", err)
	}
}

// TestKeyPath verifies that KeyPath constructs the expected path by combining
// the key directory with the "cloister-<profile>" file name.
func TestKeyPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}

	want := filepath.Join(home, ".cloister", "keys", "cloister-myagent")
	got := KeyPath("myagent")
	if got != want {
		t.Errorf("KeyPath(\"myagent\") = %q, want %q", got, want)
	}
}

// TestGenerateKey_Idempotent verifies that calling GenerateKey a second time
// for the same profile returns the original public key without overwriting the
// private key file. This guards against accidental key rotation that would
// invalidate already-deployed authorized_keys entries.
func TestGenerateKey_Idempotent(t *testing.T) {
	dir := t.TempDir()
	profile := "idempotent-profile"

	origKeyDir := overrideKeyDir(dir)
	defer restoreKeyDir(origKeyDir)

	// First call — generates fresh keypair.
	privPath1, pubKey1, err := GenerateKey(profile)
	if err != nil {
		t.Fatalf("first GenerateKey call failed: %v", err)
	}

	// Capture the modification time of the private key after the first call.
	info1, err := os.Stat(privPath1)
	if err != nil {
		t.Fatalf("stat after first GenerateKey: %v", err)
	}
	mtime1 := info1.ModTime()

	// Second call — must not regenerate the keypair.
	privPath2, pubKey2, err := GenerateKey(profile)
	if err != nil {
		t.Fatalf("second GenerateKey call failed: %v", err)
	}

	if privPath1 != privPath2 {
		t.Errorf("private key path changed between calls: %q → %q", privPath1, privPath2)
	}

	if pubKey1 != pubKey2 {
		t.Errorf("public key changed between calls, indicating an unexpected key rotation")
	}

	// Modification time must not have changed, confirming the file was not
	// rewritten during the second call.
	info2, err := os.Stat(privPath2)
	if err != nil {
		t.Fatalf("stat after second GenerateKey: %v", err)
	}
	if info2.ModTime() != mtime1 {
		t.Errorf("private key mtime changed: %v → %v; key was unexpectedly regenerated", mtime1, info2.ModTime())
	}
}

// overrideKeyDir patches keyDirOverride so that key generation writes into the
// supplied directory instead of ~/.cloister/keys. It returns the previous
// override value so the caller can restore it via restoreKeyDir.
func overrideKeyDir(dir string) string {
	prev := keyDirOverride
	keyDirOverride = dir
	return prev
}

// restoreKeyDir resets keyDirOverride to the value saved by overrideKeyDir.
func restoreKeyDir(prev string) {
	keyDirOverride = prev
}

// min returns the smaller of two integers. Introduced as a local helper
// because the built-in min was added in Go 1.21 and the module may target
// an earlier toolchain.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
