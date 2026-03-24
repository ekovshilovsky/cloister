package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MigrateState converts legacy flat-file profile state into the consolidated
// JSON format introduced in Task 2. The function is idempotent: when a JSON
// state file for the given profile already exists in stateDir the function
// returns immediately without reading or removing any flat files.
//
// Flat-file patterns consumed:
//
//	<profile>.agent.container        → ProfileState.Agent.ContainerID
//	<profile>.forward.<port>.pid     → ProfileState.Tunnels (name="agent-forward")
//	tunnel-<name>-<profile>.pid      → ProfileState.Tunnels (name=<name>)
//	<profile>.last_entry             → ProfileState.VM.LastEntry
//
// The Backend field is always set to "colima" for migrated profiles because
// Lume-backed profiles do not produce legacy flat files.
func MigrateState(profile, stateDir string) error {
	jsonPath := filepath.Join(stateDir, profile+".json")

	// Skip migration when the JSON state file already exists so that
	// subsequent calls are safe to make without clobbering live state.
	if _, err := os.Stat(jsonPath); err == nil {
		return nil
	}

	state := &ProfileState{
		// Colima is the only backend that ever wrote flat files; Lume
		// profiles always start with the JSON model.
		Backend: "colima",
	}

	// --- <profile>.agent.container -------------------------------------------
	containerPath := filepath.Join(stateDir, profile+".agent.container")
	if data, err := os.ReadFile(containerPath); err == nil {
		state.Agent.ContainerID = strings.TrimSpace(string(data))
	}

	// --- <profile>.forward.<port>.pid ----------------------------------------
	// A profile may have at most one agent-forward tunnel; glob for any port.
	forwardPattern := filepath.Join(stateDir, profile+".forward.*.pid")
	forwardMatches, _ := filepath.Glob(forwardPattern)
	for _, match := range forwardMatches {
		// Extract the port number from the filename segment between the two dots.
		base := filepath.Base(match)
		// base format: <profile>.forward.<port>.pid
		parts := strings.Split(base, ".")
		// parts: [<profile>, "forward", "<port>", "pid"]
		if len(parts) < 4 {
			continue
		}
		port, err := strconv.Atoi(parts[len(parts)-2])
		if err != nil {
			continue
		}
		pid := readPIDFile(match)
		state.Tunnels = append(state.Tunnels, TunnelState{
			Name:     "agent-forward",
			VMPort:   port,
			HostPort: port,
			SSHPID:   pid,
		})
	}

	// --- tunnel-<name>-<profile>.pid -----------------------------------------
	// Named tunnels (e.g. ollama) use a prefix-based naming convention.
	namedPattern := filepath.Join(stateDir, "tunnel-*-"+profile+".pid")
	namedMatches, _ := filepath.Glob(namedPattern)
	for _, match := range namedMatches {
		base := filepath.Base(match)
		// base format: tunnel-<name>-<profile>.pid
		// Strip the leading "tunnel-" prefix and the trailing "-<profile>.pid" suffix.
		withoutPrefix := strings.TrimPrefix(base, "tunnel-")
		suffix := "-" + profile + ".pid"
		if !strings.HasSuffix(withoutPrefix, suffix) {
			continue
		}
		name := strings.TrimSuffix(withoutPrefix, suffix)
		if name == "" {
			continue
		}
		pid := readPIDFile(match)
		state.Tunnels = append(state.Tunnels, TunnelState{
			Name:   name,
			SSHPID: pid,
		})
	}

	// --- <profile>.last_entry ------------------------------------------------
	lastEntryPath := filepath.Join(stateDir, profile+".last_entry")
	if data, err := os.ReadFile(lastEntryPath); err == nil {
		state.VM.LastEntry = strings.TrimSpace(string(data))
	}

	// Persist the consolidated state atomically before removing the flat files.
	if err := SaveState(jsonPath, state); err != nil {
		return fmt.Errorf("writing migrated state for profile %q: %w", profile, err)
	}

	// Remove flat files only after the JSON has been safely committed to disk.
	removeGlob(filepath.Join(stateDir, profile+".agent.container"))
	removeGlob(filepath.Join(stateDir, profile+".forward.*.pid"))
	removeGlob(filepath.Join(stateDir, "tunnel-*-"+profile+".pid"))
	removeGlob(filepath.Join(stateDir, profile+".last_entry"))

	return nil
}

// readPIDFile reads the file at path, trims surrounding whitespace, and parses
// the content as a decimal integer PID. It returns 0 when the file cannot be
// read or the content is not a valid integer, allowing callers to treat a
// missing or corrupt PID file as an absent (dead) process.
func readPIDFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// removeGlob expands pattern using filepath.Glob and removes every matched
// path. Individual removal errors are silently ignored so that missing files
// (already cleaned up by another process) do not surface as failures.
func removeGlob(pattern string) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, match := range matches {
		_ = os.Remove(match)
	}
}
