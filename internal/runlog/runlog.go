// Proprietary and confidential. All rights reserved.

// Package runlog records the full output of one cloister command to a file so
// the console does not have to carry it.
//
// Provisioning produces hundreds of lines of package-manager output. Streaming
// it to the terminal shows activity, which is worth something, but it buries
// the few lines that report what actually happened and asks the reader to tell
// signal from noise in a scrolling wall. Keeping the whole stream on disk lets
// the console show progress instead, without losing anything needed to
// diagnose a failure afterwards.
package runlog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultRetention is how many runs are kept for a profile. Runs are bounded
// and infrequent, so a count is easier to reason about than a size budget:
// "the last twenty repairs" needs no arithmetic to predict.
const DefaultRetention = 20

// Run is the log file for a single command invocation.
type Run struct {
	path string
	file *os.File
}

// Open starts a run log for the given profile and command under dir, pruning
// that profile's older logs to DefaultRetention.
func Open(dir, profile, command string) (*Run, error) {
	return OpenWithRetention(dir, profile, command, DefaultRetention)
}

// OpenWithRetention is Open with an explicit retention count. A retention of
// zero or less keeps everything.
func OpenWithRetention(dir, profile, command string, retention int) (*Run, error) {
	if err := validSegment("profile", profile); err != nil {
		return nil, err
	}
	if err := validSegment("command", command); err != nil {
		return nil, err
	}

	// Each profile gets a directory rather than a filename prefix. Profile
	// names contain dashes, so pruning by prefix would let a profile named
	// "battery" delete the logs of "battery-1800".
	profileDir := filepath.Join(dir, profile)
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the run log directory: %w", err)
	}

	// Prune before creating, so the new run is the one the limit makes room
	// for rather than the one that pushes the count over it.
	if retention > 0 {
		if err := prune(profileDir, retention-1); err != nil {
			return nil, err
		}
	}

	path := filepath.Join(profileDir, command+"-"+time.Now().Format("20060102-150405")+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating the run log: %w", err)
	}
	return &Run{path: path, file: file}, nil
}

// Writer returns the sink for this run's output.
func (r *Run) Writer() io.Writer {
	return r.file
}

// Path is the location of the run log, for reporting to the user.
func (r *Run) Path() string {
	return r.path
}

// Close releases the run log. Calling it more than once is not an error, so a
// deferred Close can sit alongside an explicit one.
func (r *Run) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// prune removes the oldest logs until at most keep remain. Only files this
// package creates are considered, so anything else in the directory survives.
func prune(profileDir string, keep int) error {
	if keep < 0 {
		keep = 0
	}
	entries, err := filepath.Glob(filepath.Join(profileDir, "*.log"))
	if err != nil {
		return fmt.Errorf("listing existing run logs: %w", err)
	}
	// Only files named the way Open names them are candidates. Sorting the
	// bare filenames would order by command before time, because the command
	// comes first and "create" sorts ahead of "repair"; the timestamp is what
	// decides age, so it is what the ordering uses.
	type candidate struct {
		path      string
		timestamp string
	}
	candidates := make([]candidate, 0, len(entries))
	for _, path := range entries {
		timestamp, ok := logTimestamp(filepath.Base(path))
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{path: path, timestamp: timestamp})
	}
	if len(candidates) <= keep {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].timestamp != candidates[j].timestamp {
			return candidates[i].timestamp < candidates[j].timestamp
		}
		return candidates[i].path < candidates[j].path
	})
	for _, stale := range candidates[:len(candidates)-keep] {
		if err := os.Remove(stale.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("pruning run log %q: %w", stale.path, err)
		}
	}
	return nil
}

// logTimestamp extracts the fixed-width "YYYYMMDD-HHMMSS" stamp Open appends
// to every run log name. A name without one was not written by this package,
// so pruning leaves it alone.
func logTimestamp(name string) (string, bool) {
	const stamp = len("20060102-150405")
	base := strings.TrimSuffix(name, ".log")
	if len(base) < stamp+1 {
		return "", false
	}
	candidate := base[len(base)-stamp:]
	if base[len(base)-stamp-1] != '-' {
		return "", false
	}
	for i, r := range candidate {
		if i == 8 {
			if r != '-' {
				return "", false
			}
			continue
		}
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return candidate, true
}

// validSegment rejects names that cannot be used as a single path segment.
// Both values name a directory whose contents pruning deletes, so a traversal
// here would delete files elsewhere.
func validSegment(field, value string) error {
	if value == "" {
		return fmt.Errorf("run log %s is empty", field)
	}
	if value == "." || value == ".." ||
		strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("run log %s %q is not a usable path segment", field, value)
	}
	return nil
}
