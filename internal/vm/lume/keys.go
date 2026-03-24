package lume

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// keyDirOverride is non-empty only during tests. When set, KeyDir returns this
// value instead of the real ~/.cloister/keys path, allowing tests to redirect
// key generation to an isolated temporary directory.
var keyDirOverride string

// KeyDir returns the directory used to store cloister-managed SSH keys for
// Lume-backed VMs. All keys are isolated from the user's general SSH directory
// to avoid polluting ~/.ssh with hypervisor-specific material.
func KeyDir() string {
	if keyDirOverride != "" {
		return keyDirOverride
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cloister", "keys")
}

// KeyPath returns the absolute path to the Ed25519 private key file for the
// given cloister profile. The corresponding public key is stored at the same
// path with a ".pub" suffix appended.
func KeyPath(profile string) string {
	return filepath.Join(KeyDir(), "cloister-"+profile)
}

// GenerateKey creates an Ed25519 SSH keypair for the given profile and stores
// it under ~/.cloister/keys/. The private key is written with mode 0600 and
// the public key with mode 0644.
//
// The function is idempotent: if a private key already exists at the expected
// path, the existing public key is returned without regenerating either file.
// This ensures that repeated provisioning calls do not rotate the key and
// invalidate any authorized_keys entries already deployed to the VM.
func GenerateKey(profile string) (privateKeyPath string, publicKey string, err error) {
	privPath := KeyPath(profile)
	pubPath := privPath + ".pub"

	// Return the existing keypair when the private key file is already present,
	// preserving any authorized_keys entries already deployed to the VM.
	if _, statErr := os.Stat(privPath); statErr == nil {
		pubData, readErr := os.ReadFile(pubPath)
		if readErr == nil {
			return privPath, string(pubData), nil
		}
		// Public key file is missing despite the private key existing; fall
		// through to regenerate both files from a fresh keypair.
	}

	// Generate a new Ed25519 keypair. Ed25519 is preferred over RSA for Lume
	// VMs because it produces compact keys, signs quickly, and is the modern
	// standard for OpenSSH deployments.
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generating Ed25519 keypair: %w", err)
	}

	// Serialize the private key to OpenSSH PEM format so that the standard
	// ssh(1) client can consume it via the -i flag without additional tooling.
	privPEM, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return "", "", fmt.Errorf("marshaling private key to PEM: %w", err)
	}
	privPEMBytes := pem.EncodeToMemory(privPEM)

	// Serialize the public key to the authorized_keys line format
	// (e.g. "ssh-ed25519 AAAA...") so it can be appended directly to the
	// guest's ~/.ssh/authorized_keys file.
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return "", "", fmt.Errorf("creating SSH public key object: %w", err)
	}
	pubKeyStr := string(ssh.MarshalAuthorizedKey(sshPub))

	// Ensure the key directory exists with restrictive permissions before
	// writing key material; 0700 prevents other users from listing contents.
	if err := os.MkdirAll(filepath.Dir(privPath), 0o700); err != nil {
		return "", "", fmt.Errorf("creating key directory %s: %w", filepath.Dir(privPath), err)
	}

	// Write private key first, with mode 0600 (owner read/write only).
	// SSH clients reject private key files with looser permissions.
	if err := os.WriteFile(privPath, privPEMBytes, 0o600); err != nil {
		return "", "", fmt.Errorf("writing private key to %s: %w", privPath, err)
	}

	// Write the public key with standard 0644 permissions so it can be
	// read by scripts and tooling without requiring elevated access.
	if err := os.WriteFile(pubPath, []byte(pubKeyStr), 0o644); err != nil {
		return "", "", fmt.Errorf("writing public key to %s: %w", pubPath, err)
	}

	return privPath, pubKeyStr, nil
}

// DeployKey installs the given public key into the Lume VM's
// ~/.ssh/authorized_keys file so that subsequent SSH operations can
// authenticate with the cloister-managed private key rather than relying on
// password auth.
//
// Lume VMs are bootstrapped with default credentials (user: lume, password:
// lume). The `lume ssh` subcommand handles that initial password-based login
// internally, making it the only viable path for first-time key injection.
func DeployKey(vmName, publicKey string) error {
	// Construct the remote shell fragment that creates ~/.ssh if absent,
	// appends the public key, and tightens permissions to what sshd requires.
	remoteCmd := fmt.Sprintf(
		"mkdir -p ~/.ssh && echo '%s' >> ~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys",
		strings.TrimSpace(publicKey),
	)

	cmd := exec.Command("lume", "ssh", vmName, remoteCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("deploying SSH key to Lume VM %s: %w\n%s", vmName, err, string(out))
	}
	return nil
}
