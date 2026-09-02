package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"cloister.io/internal/lifecycle"
	"cloister.io/internal/runlog"
)

// attachPreflightLog gives the coordinator somewhere to keep the per-file
// record of the broker metadata preflight, and returns the function that
// releases it.
//
// Entering a workspace collection inspects every included file in every
// project. The console reports what that found; the file lists behind those
// counts belong where they can be read afterwards rather than scrolled past on
// the way to a shell.
func attachPreflightLog(coordinator *lifecycle.Coordinator, profile string) func() {
	dir, err := preflightLogDir()
	if err == nil {
		var run *runlog.Run
		if run, err = runlog.Open(dir, profile, "preflight"); err == nil {
			coordinator.MetadataLog = run.Writer()
			coordinator.MetadataLogPath = run.Path()
			return func() { run.Close() }
		}
	}
	// The console summary does not depend on the record, so a log that cannot
	// be opened costs the file list rather than the entry.
	fmt.Fprintf(os.Stderr, "warning: no workspace metadata log for this entry: %v\n", err)
	return func() {}
}

// preflightLogDir keeps entry records out of the provisioning log directory.
// Retention is per directory, and an entry happens many times for every
// provision, so a shared budget would let entries evict the provisioning logs a
// failed create is diagnosed from.
func preflightLogDir() (string, error) {
	dir, err := logDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "workspace"), nil
}
