package cmd

import (
	"fmt"
	"os"

	"cloister.io/internal/vm"
)

// startVM starts the VM for a profile, wrapping backend.Start with detection and
// recovery for the stale disk-lock failure that follows an unclean shutdown
// (e.g. a host crash). All cloister start paths route through this helper so the
// behavior is consistent everywhere.
//
// On a start failure it asks the backend (if it supports it) to diagnose a stale
// lock. When one is found:
//   - Interactive terminal: explain the cause, prompt for confirmation, clear
//     the lock, and retry the start once.
//   - Non-interactive (agent/CI): explain the cause and point the user at
//     `cloister cleanup`, then return the original error without killing
//     anything.
//
// Any failure that is not a stale lock is returned unchanged.
func startVM(backend vm.Backend, profile string, cpus, memoryGB, diskGB, rootDiskGB int, mountInotify bool, mounts []vm.Mount, verbose bool) error {
	err := backend.Start(profile, cpus, memoryGB, diskGB, rootDiskGB, mountInotify, mounts, verbose)
	if err == nil {
		return nil
	}

	recoverer, ok := backend.(vm.StaleLockRecoverer)
	if !ok {
		return err
	}
	diag := recoverer.DiagnoseStartFailure(profile)
	if diag == nil {
		return err
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, diag.Summary)

	if !isInteractive() {
		fmt.Fprintln(os.Stderr, "Run 'cloister cleanup' to clear the stale lock, then retry.")
		return err
	}

	if !promptYesNo("Recover now and retry the start? [Y/n] ") {
		fmt.Fprintln(os.Stderr, "Run 'cloister cleanup' to clear the stale lock, then retry.")
		return err
	}

	cleared, cerr := recoverer.ClearStaleLock(profile)
	if cerr != nil {
		return fmt.Errorf("stale-lock recovery failed: %w", cerr)
	}
	fmt.Printf("Cleared stale lock (terminated %d orphaned process(es)). Retrying start...\n", cleared)

	return backend.Start(profile, cpus, memoryGB, diskGB, rootDiskGB, mountInotify, mounts, verbose)
}
