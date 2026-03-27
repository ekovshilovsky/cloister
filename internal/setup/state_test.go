package setup_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ekovshilovsky/cloister/internal/setup"
)

// buildState returns a populated SetupState suitable for round-trip assertions.
func buildState() *setup.SetupState {
	return &setup.SetupState{
		Version:         1,
		CredentialStore: "op",
		Credentials: setup.CredentialState{
			AnthropicAPIKey: true,
			TelegramBotToken: true,
		},
		Channels: setup.ChannelState{
			Telegram: setup.TelegramState{
				Configured:  true,
				BotUsername: "my_bot",
			},
			WhatsApp: setup.WhatsAppState{
				Configured: true,
				Mode:       "action-only",
				Number:     "+15550001234",
			},
		},
		Providers: setup.ProviderState{
			Ollama: setup.OllamaState{
				Configured:   true,
				Host:         "http://localhost:11434",
				PrimaryModel: "llama3",
			},
			Anthropic:       setup.AnthropicState{Configured: true},
			DefaultProvider: "anthropic",
		},
		Pairing: setup.PairingState{
			NodeHostRegistered: true,
			NodeDisplayName:    "dev-mac",
			DevicesApproved:    true,
		},
		OAuth: setup.OAuthState{
			GoogleServices: []string{"calendar", "gmail"},
		},
	}
}

// TestSetupStateRoundTrip verifies that a SetupState can be saved to disk and
// loaded back with all fields intact.
func TestSetupStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	original := buildState()

	if err := setup.SaveState(path, original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := setup.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	if loaded.Version != original.Version {
		t.Errorf("Version: got %d, want %d", loaded.Version, original.Version)
	}
	if loaded.CredentialStore != original.CredentialStore {
		t.Errorf("CredentialStore: got %q, want %q", loaded.CredentialStore, original.CredentialStore)
	}
	if !loaded.Credentials.AnthropicAPIKey {
		t.Error("Credentials.AnthropicAPIKey: expected true")
	}
	if !loaded.Credentials.TelegramBotToken {
		t.Error("Credentials.TelegramBotToken: expected true")
	}
	if loaded.Channels.Telegram.BotUsername != "my_bot" {
		t.Errorf("Telegram.BotUsername: got %q, want %q", loaded.Channels.Telegram.BotUsername, "my_bot")
	}
	if loaded.Channels.WhatsApp.Number != "+15550001234" {
		t.Errorf("WhatsApp.Number: got %q, want %q", loaded.Channels.WhatsApp.Number, "+15550001234")
	}
	if loaded.Providers.Ollama.PrimaryModel != "llama3" {
		t.Errorf("Ollama.PrimaryModel: got %q, want %q", loaded.Providers.Ollama.PrimaryModel, "llama3")
	}
	if loaded.Providers.DefaultProvider != "anthropic" {
		t.Errorf("DefaultProvider: got %q, want %q", loaded.Providers.DefaultProvider, "anthropic")
	}
	if loaded.Pairing.NodeDisplayName != "dev-mac" {
		t.Errorf("Pairing.NodeDisplayName: got %q, want %q", loaded.Pairing.NodeDisplayName, "dev-mac")
	}
	if len(loaded.OAuth.GoogleServices) != 2 {
		t.Errorf("OAuth.GoogleServices: got %d entries, want 2", len(loaded.OAuth.GoogleServices))
	}
	if loaded.CreatedAt.IsZero() {
		t.Error("CreatedAt must be set after SaveState")
	}
	if loaded.UpdatedAt.IsZero() {
		t.Error("UpdatedAt must be set after SaveState")
	}
}

// TestLoadStateMissingFile verifies that loading a nonexistent state file
// returns a zero-valued SetupState without an error.
func TestLoadStateMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.yaml")

	state, err := setup.LoadState(path)
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil zero state")
	}
	if state.Version != 0 {
		t.Errorf("expected zero Version, got %d", state.Version)
	}
	if state.CredentialStore != "" {
		t.Errorf("expected empty CredentialStore, got %q", state.CredentialStore)
	}
}

// TestProgressRoundTrip verifies that a Progress value can be saved and
// reloaded with all fields intact.
func TestProgressRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.progress")

	original := &setup.Progress{
		CurrentSection: "credentials",
		CurrentStep:    "keychain",
		CompletedSteps: []string{"preflight:check", "credentials:keychain"},
	}

	if err := setup.SaveProgress(path, original); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}

	loaded, err := setup.LoadProgress(path)
	if err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}

	if loaded.CurrentSection != original.CurrentSection {
		t.Errorf("CurrentSection: got %q, want %q", loaded.CurrentSection, original.CurrentSection)
	}
	if loaded.CurrentStep != original.CurrentStep {
		t.Errorf("CurrentStep: got %q, want %q", loaded.CurrentStep, original.CurrentStep)
	}
	if len(loaded.CompletedSteps) != len(original.CompletedSteps) {
		t.Errorf("CompletedSteps length: got %d, want %d", len(loaded.CompletedSteps), len(original.CompletedSteps))
	}
}

// TestProgressMarkComplete verifies that MarkComplete appends the composite
// key to CompletedSteps, advances the current position, and reports the step
// as complete via IsComplete.
func TestProgressMarkComplete(t *testing.T) {
	p := &setup.Progress{}

	p.MarkComplete("credentials", "keychain")

	if !p.IsComplete("credentials", "keychain") {
		t.Error("IsComplete should return true after MarkComplete")
	}
	if p.CurrentSection != "credentials" {
		t.Errorf("CurrentSection: got %q, want %q", p.CurrentSection, "credentials")
	}
	if p.CurrentStep != "keychain" {
		t.Errorf("CurrentStep: got %q, want %q", p.CurrentStep, "keychain")
	}
	if len(p.CompletedSteps) != 1 {
		t.Errorf("CompletedSteps length: got %d, want 1", len(p.CompletedSteps))
	}
	if p.FailedStep != nil {
		t.Error("MarkComplete should clear FailedStep")
	}

	// Ensure a different section/step is not reported as complete.
	if p.IsComplete("channels", "telegram") {
		t.Error("IsComplete should return false for an unmarked step")
	}
}

// TestProgressMarkFailed verifies that MarkFailed populates FailedStep with
// the correct section, step, error message, and a non-zero timestamp.
func TestProgressMarkFailed(t *testing.T) {
	p := &setup.Progress{}

	before := time.Now()
	p.MarkFailed("channels", "telegram", "bot token rejected")
	after := time.Now()

	if p.FailedStep == nil {
		t.Fatal("FailedStep must be non-nil after MarkFailed")
	}
	if p.FailedStep.Section != "channels" {
		t.Errorf("FailedStep.Section: got %q, want %q", p.FailedStep.Section, "channels")
	}
	if p.FailedStep.Step != "telegram" {
		t.Errorf("FailedStep.Step: got %q, want %q", p.FailedStep.Step, "telegram")
	}
	if p.FailedStep.Error != "bot token rejected" {
		t.Errorf("FailedStep.Error: got %q, want %q", p.FailedStep.Error, "bot token rejected")
	}
	if p.FailedStep.Timestamp.Before(before) || p.FailedStep.Timestamp.After(after) {
		t.Errorf("FailedStep.Timestamp %v outside expected range [%v, %v]",
			p.FailedStep.Timestamp, before, after)
	}
}

// TestClearProgress verifies that ClearProgress removes a previously saved
// progress file.
func TestClearProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.progress")

	p := &setup.Progress{CurrentSection: "credentials", CurrentStep: "keychain"}
	if err := setup.SaveProgress(path, p); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}

	if err := setup.ClearProgress(path); err != nil {
		t.Fatalf("ClearProgress: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("progress file should not exist after ClearProgress")
	}
}

// TestClearProgressMissingFile verifies that ClearProgress returns no error
// when the target file does not exist.
func TestClearProgressMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.progress")

	if err := setup.ClearProgress(path); err != nil {
		t.Fatalf("ClearProgress on missing file should not error, got: %v", err)
	}
}

// TestSaveStateAtomicWrite verifies that no .tmp file is left behind after a
// successful SaveState call, confirming the atomic rename completed cleanly.
func TestSaveStateAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	state := buildState()
	if err := setup.SaveState(path, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temporary file %s should not exist after SaveState", tmpPath)
	}

	// Confirm the final file was written.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected state file to exist at %s: %v", path, err)
	}
}
