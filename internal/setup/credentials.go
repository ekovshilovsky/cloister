package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CredentialStore abstracts credential storage. Implementations provide either
// 1Password integration (OpStore) or local file storage (LocalStore). Used by
// all wizard sections and shared across the codebase by the provisioning
// engine, repair, and rebuild commands.
type CredentialStore interface {
	Get(profile, key string) (string, error)
	Set(profile, key, value string) error
	Has(profile, key string) bool
}

// LocalStore stores credentials as individual files in a directory tree:
// <baseDir>/<profile>/<key> with mode 0600.
type LocalStore struct {
	baseDir string
}

// NewLocalStore creates a LocalStore rooted at the given directory.
func NewLocalStore(baseDir string) *LocalStore {
	return &LocalStore{baseDir: baseDir}
}

// NewDefaultLocalStore creates a LocalStore at ~/.cloister/keys/.
func NewDefaultLocalStore() (*LocalStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return NewLocalStore(filepath.Join(home, ".cloister", "keys")), nil
}

// Get reads the credential value for the given profile and key.
func (s *LocalStore) Get(profile, key string) (string, error) {
	path := filepath.Join(s.baseDir, profile, key)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("credential %q not found for profile %q", key, profile)
	}
	return strings.TrimSpace(string(data)), nil
}

// Set writes the credential value for the given profile and key. Creates the
// profile directory if needed. Files are written with mode 0600.
func (s *LocalStore) Set(profile, key, value string) error {
	dir := filepath.Join(s.baseDir, profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, key), []byte(value), 0o600)
}

// Has reports whether a credential exists for the given profile and key.
func (s *LocalStore) Has(profile, key string) bool {
	_, err := os.Stat(filepath.Join(s.baseDir, profile, key))
	return err == nil
}

// OpStore stores credentials in 1Password using the op CLI. Items are named
// cloister-<profile>-<key> in the Private vault.
type OpStore struct{}

// NewOpStore creates an OpStore. Verify op is available with IsOpAvailable first.
func NewOpStore() *OpStore {
	return &OpStore{}
}

func opItemName(profile, key string) string {
	return fmt.Sprintf("cloister-%s-%s", profile, key)
}

// Get retrieves a credential from 1Password.
func (s *OpStore) Get(profile, key string) (string, error) {
	name := opItemName(profile, key)
	out, err := exec.Command("op", "item", "get", name, "--fields", "password", "--reveal").Output()
	if err != nil {
		return "", fmt.Errorf("1Password: credential %q not found for profile %q", key, profile)
	}
	return strings.TrimSpace(string(out)), nil
}

// Set stores a credential in 1Password. Updates an existing item or creates
// a new one in the Private vault.
func (s *OpStore) Set(profile, key, value string) error {
	name := opItemName(profile, key)
	if err := exec.Command("op", "item", "edit", name, "password="+value).Run(); err == nil {
		return nil
	}
	return exec.Command("op", "item", "create",
		"--category=password",
		"--title="+name,
		"--vault=Private",
		"password="+value,
	).Run()
}

// Has reports whether a credential exists in 1Password.
func (s *OpStore) Has(profile, key string) bool {
	name := opItemName(profile, key)
	return exec.Command("op", "item", "get", name, "--fields", "label").Run() == nil
}

// IsOpAvailable returns true when the 1Password CLI (op) is installed and
// accessible on the PATH.
func IsOpAvailable() bool {
	_, err := exec.LookPath("op")
	return err == nil
}
