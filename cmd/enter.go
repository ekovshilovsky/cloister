package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// enterProfile is the primary user interaction for cloister. It starts the VM
// for the named profile if it is not already running, records the entry
// timestamp for idle-time tracking, and then drops the user into an interactive
// SSH session inside the VM.
func enterProfile(name string) error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	p, ok := cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("profile %q not found. Create it with: cloister create %s", name, name)
	}

	// Ensure any zero-value resource fields are filled in with package defaults
	// before they are passed to the VM layer.
	p.ApplyDefaults()

	if !vm.IsRunning(name) {
		fmt.Printf("Starting %q...\n", name)

		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}

		mounts := vm.BuildMounts(home)

		if err := vm.Start(name, p.CPU, p.Memory, p.Disk, mounts, false); err != nil {
			return fmt.Errorf("starting VM for profile %q: %w", name, err)
		}
	}

	// TODO(task-6): Set terminal identity (accent color, window title) once
	// the terminal integration layer is implemented.

	// Record the current Unix timestamp so that the status command can
	// calculate how long ago this profile was last entered.
	if err := writeLastEntryTimestamp(name); err != nil {
		// Non-fatal: idle tracking is best-effort and should not block entry.
		fmt.Fprintf(os.Stderr, "warning: could not record entry timestamp: %v\n", err)
	}

	fmt.Printf("Entering %s...\n", name)
	return vm.SSH(name)
}

// writeLastEntryTimestamp persists the current Unix timestamp to
// ~/.cloister/state/<profile>.last_entry. The file is used by the status
// command to compute the idle duration for each profile.
func writeLastEntryTimestamp(profile string) error {
	dir, err := config.ConfigDir()
	if err != nil {
		return fmt.Errorf("resolving config dir: %w", err)
	}

	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path := filepath.Join(stateDir, profile+".last_entry")
	return os.WriteFile(path, []byte(ts), 0o600)
}
