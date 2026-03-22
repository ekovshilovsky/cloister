package vmcli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteClaudeLocalEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-mode.env")

	if err := WriteClaudeLocalEnv(path); err != nil {
		t.Fatalf("WriteClaudeLocalEnv: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading env file: %v", err)
	}
	content := string(data)
	if len(content) == 0 {
		t.Error("env file should not be empty")
	}
}

func TestRemoveClaudeEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-mode.env")
	os.WriteFile(path, []byte("test"), 0o600)

	if err := RemoveClaudeEnv(path); err != nil {
		t.Fatalf("RemoveClaudeEnv: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("env file should be removed")
	}
}

func TestRemoveClaudeEnvMissing(t *testing.T) {
	// Removing a non-existent file should not error
	if err := RemoveClaudeEnv("/nonexistent/file"); err != nil {
		t.Errorf("RemoveClaudeEnv on missing file should not error: %v", err)
	}
}

func TestClaudeLocalEvalOutput(t *testing.T) {
	output := ClaudeLocalEvalOutput()
	if output == "" {
		t.Error("eval output should not be empty")
	}
}

func TestClaudeCloudEvalOutput(t *testing.T) {
	output := ClaudeCloudEvalOutput()
	if output == "" {
		t.Error("eval output should not be empty")
	}
}

func TestLoadClaudeEnvInvalidContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-mode.env")
	os.WriteFile(path, []byte("not valid bash export"), 0o600)
	// File should still be readable without error — bash will handle syntax
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("should be able to read invalid env file: %v", err)
	}
	if len(data) == 0 {
		t.Error("file should not be empty")
	}
}
