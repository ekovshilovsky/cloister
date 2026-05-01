// Package gpgforward holds shared helpers for cloister's GPG agent forwarding
// feature. The package only exists so that the `cmd` package and the linux
// provisioning engine can both reach the same state-file conventions without
// importing each other.
package gpgforward

import (
	"os"
	"path/filepath"
	"strings"
)

// stateFile is the path under the user's home where cloister persists the
// resolved host extra-socket path. The provisioning engine reads this file
// when starting the reverse-forwarded socket tunnel; `cloister setup
// gpg-forward` writes it.
const stateFile = ".cloister/state/gpg-forward-host-socket"

// PersistHostSocketPath writes the resolved gpg-agent extra-socket path to
// the cloister state directory. Caller is responsible for ensuring path is
// non-empty and points at a real socket.
func PersistHostSocketPath(home, socketPath string) error {
	state := filepath.Join(home, stateFile)
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		return err
	}
	return os.WriteFile(state, []byte(socketPath+"\n"), 0o600)
}

// LoadHostSocketPath reads the persisted gpg-agent extra-socket path. Returns
// an empty string with no error when the file does not exist (i.e. the user
// has not yet run `cloister setup gpg-forward`).
func LoadHostSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, stateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
