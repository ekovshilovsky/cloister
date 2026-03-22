package vmcli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte(`{
		"profile": "work",
		"tunnels": [{"name": "ollama", "port": 11434}],
		"workspace": "/Users/user/code",
		"claude_local": true
	}`), 0o600)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Profile != "work" {
		t.Errorf("Profile = %q, want %q", cfg.Profile, "work")
	}
	if !cfg.ClaudeLocal {
		t.Error("ClaudeLocal should be true")
	}
}

func TestLoadConfigMissing(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.json")
	if err == nil {
		t.Error("expected error for missing config")
	}
}

func TestLoadConfigMalformed(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte(`{invalid json`), 0o600)

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Error("expected error for malformed config")
	}
}
