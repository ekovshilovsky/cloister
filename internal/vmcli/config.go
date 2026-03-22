package vmcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ekovshilovsky/cloister/internal/vmconfig"
)

// DefaultConfigPath returns the standard location for the VM-side config file,
// which is written by the host provisioning engine during VM setup.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cloister-vm", "config.json")
}

// LoadConfig reads and parses the VM-side config file at the given path.
// If the file is missing or malformed, the error message directs the user
// to re-run provisioning from the host to regenerate it.
func LoadConfig(path string) (*vmconfig.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config file not found at %s\nRun 'cloister rebuild <profile>' from the host to regenerate it", path)
	}

	var cfg vmconfig.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config file at %s is malformed: %w\nRun 'cloister rebuild <profile>' from the host to regenerate it", path, err)
	}

	return &cfg, nil
}
