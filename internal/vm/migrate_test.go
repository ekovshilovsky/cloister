package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigrateState_FullMigration seeds a temporary directory with all four
// flat-file state patterns and verifies that MigrateState produces a single
// consolidated JSON file with the correct field values, then removes every
// flat file that was consumed.
func TestMigrateState_FullMigration(t *testing.T) {
	dir := t.TempDir()

	// Seed the flat files that predate the JSON state model.
	files := map[string]string{
		"myprofile.agent.container":      "abc123def456",
		"myprofile.forward.18789.pid":    "9182",
		"tunnel-ollama-myprofile.pid":    "7234",
		"myprofile.last_entry":           "2026-03-24T10:00:00Z",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	if err := MigrateState("myprofile", dir); err != nil {
		t.Fatalf("MigrateState: %v", err)
	}

	// Verify the consolidated JSON file was written.
	jsonPath := filepath.Join(dir, "myprofile.json")
	state, err := LoadState(jsonPath)
	if err != nil {
		t.Fatalf("LoadState after migration: %v", err)
	}

	if state.Backend != "colima" {
		t.Errorf("Backend: got %q, want %q", state.Backend, "colima")
	}
	if state.Agent.ContainerID != "abc123def456" {
		t.Errorf("Agent.ContainerID: got %q, want %q", state.Agent.ContainerID, "abc123def456")
	}
	if state.VM.LastEntry != "2026-03-24T10:00:00Z" {
		t.Errorf("VM.LastEntry: got %q, want %q", state.VM.LastEntry, "2026-03-24T10:00:00Z")
	}

	// Locate the agent-forward tunnel (from <profile>.forward.<port>.pid).
	var forwardTunnel *TunnelState
	var ollamaTunnel *TunnelState
	for i := range state.Tunnels {
		switch state.Tunnels[i].Name {
		case "agent-forward":
			forwardTunnel = &state.Tunnels[i]
		case "ollama":
			ollamaTunnel = &state.Tunnels[i]
		}
	}

	if forwardTunnel == nil {
		t.Fatal("tunnels: no entry with name=\"agent-forward\"")
	}
	if forwardTunnel.VMPort != 18789 {
		t.Errorf("agent-forward VMPort: got %d, want 18789", forwardTunnel.VMPort)
	}
	if forwardTunnel.SSHPID != 9182 {
		t.Errorf("agent-forward SSHPID: got %d, want 9182", forwardTunnel.SSHPID)
	}

	if ollamaTunnel == nil {
		t.Fatal("tunnels: no entry with name=\"ollama\"")
	}
	if ollamaTunnel.SSHPID != 7234 {
		t.Errorf("ollama SSHPID: got %d, want 7234", ollamaTunnel.SSHPID)
	}

	// All four flat files must be removed after a successful migration.
	for name := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("flat file %s was not removed after migration", name)
		}
	}
}

// TestMigrateState_Idempotent verifies that calling MigrateState when a JSON
// state file already exists is a no-op: the function returns without error and
// does not overwrite the existing JSON content.
func TestMigrateState_Idempotent(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "myprofile.json")

	// Write a pre-existing JSON state file with a sentinel backend value.
	existing := &ProfileState{Backend: "lume"}
	if err := SaveState(jsonPath, existing); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Also place a flat file to confirm it is not consumed.
	flatPath := filepath.Join(dir, "myprofile.agent.container")
	if err := os.WriteFile(flatPath, []byte("shouldnotmigrate"), 0o600); err != nil {
		t.Fatalf("seeding flat file: %v", err)
	}

	if err := MigrateState("myprofile", dir); err != nil {
		t.Fatalf("MigrateState (idempotent): %v", err)
	}

	state, err := LoadState(jsonPath)
	if err != nil {
		t.Fatalf("LoadState after idempotent call: %v", err)
	}
	// The JSON must not have been overwritten with the flat-file data.
	if state.Backend != "lume" {
		t.Errorf("Backend: got %q, want %q (JSON must not be overwritten)", state.Backend, "lume")
	}
	if state.Agent.ContainerID != "" {
		t.Errorf("Agent.ContainerID: got %q, want empty (flat file must not be consumed)", state.Agent.ContainerID)
	}

	// The flat file must still be present because migration was skipped.
	if _, err := os.Stat(flatPath); os.IsNotExist(err) {
		t.Error("flat file was removed even though JSON already existed")
	}
}

// TestMigrateState_NoFiles verifies that MigrateState succeeds when no flat
// files are present, producing a minimal JSON document with backend="colima"
// and no tunnel or agent fields.
func TestMigrateState_NoFiles(t *testing.T) {
	dir := t.TempDir()

	if err := MigrateState("myprofile", dir); err != nil {
		t.Fatalf("MigrateState (no files): %v", err)
	}

	jsonPath := filepath.Join(dir, "myprofile.json")
	state, err := LoadState(jsonPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Backend != "colima" {
		t.Errorf("Backend: got %q, want %q", state.Backend, "colima")
	}
	if len(state.Tunnels) != 0 {
		t.Errorf("Tunnels: got %d entries, want 0", len(state.Tunnels))
	}
	if state.Agent.ContainerID != "" {
		t.Errorf("Agent.ContainerID: got %q, want empty", state.Agent.ContainerID)
	}
	if state.VM.LastEntry != "" {
		t.Errorf("VM.LastEntry: got %q, want empty", state.VM.LastEntry)
	}
}

// TestMigrateState_PartialFiles verifies that MigrateState handles a subset of
// flat files without error, migrating only the fields that have corresponding
// files on disk and leaving absent fields at their zero values.
func TestMigrateState_PartialFiles(t *testing.T) {
	dir := t.TempDir()

	// Provide only the container file; all other flat files are absent.
	containerPath := filepath.Join(dir, "myprofile.agent.container")
	if err := os.WriteFile(containerPath, []byte("deadbeef"), 0o600); err != nil {
		t.Fatalf("seeding container file: %v", err)
	}

	if err := MigrateState("myprofile", dir); err != nil {
		t.Fatalf("MigrateState (partial): %v", err)
	}

	jsonPath := filepath.Join(dir, "myprofile.json")
	state, err := LoadState(jsonPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Backend != "colima" {
		t.Errorf("Backend: got %q, want %q", state.Backend, "colima")
	}
	if state.Agent.ContainerID != "deadbeef" {
		t.Errorf("Agent.ContainerID: got %q, want %q", state.Agent.ContainerID, "deadbeef")
	}
	if len(state.Tunnels) != 0 {
		t.Errorf("Tunnels: got %d entries, want 0", len(state.Tunnels))
	}
	if state.VM.LastEntry != "" {
		t.Errorf("VM.LastEntry: got %q, want empty", state.VM.LastEntry)
	}

	// The consumed flat file must be removed.
	if _, err := os.Stat(containerPath); !os.IsNotExist(err) {
		t.Error("container flat file was not removed after partial migration")
	}
}
