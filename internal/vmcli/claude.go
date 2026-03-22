package vmcli

import (
	"fmt"
	"os"
	"path/filepath"
)

// claudeEnvContent contains the shell export statements that redirect Claude Code
// from Anthropic's cloud API to the locally tunneled Ollama server.
const claudeEnvContent = `export ANTHROPIC_BASE_URL="http://127.0.0.1:11434"
export ANTHROPIC_AUTH_TOKEN="ollama"
export ANTHROPIC_API_KEY=""
`

// DefaultClaudeEnvPath returns the conventional path for the Claude mode env file
// within the per-user cloister-vm configuration directory.
func DefaultClaudeEnvPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cloister-vm", "claude-mode.env")
}

// WriteClaudeLocalEnv writes the local-mode env file that points Claude Code at
// the Ollama server tunneled from the macOS host. Parent directories are created
// if they do not already exist.
func WriteClaudeLocalEnv(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	return os.WriteFile(path, []byte(claudeEnvContent), 0o600)
}

// RemoveClaudeEnv deletes the Claude mode env file to restore cloud-API behavior.
// Returns nil if the file does not exist, so the operation is idempotent.
func RemoveClaudeEnv(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ClaudeLocalEvalOutput returns the shell export statements suitable for use with
// eval, enabling shell integration via: eval $(cloister-vm claude-local --eval)
func ClaudeLocalEvalOutput() string {
	return claudeEnvContent
}

// ClaudeCloudEvalOutput returns shell unset statements that remove the local-mode
// overrides, restoring default Anthropic cloud API behavior in the current shell.
func ClaudeCloudEvalOutput() string {
	return "unset ANTHROPIC_BASE_URL ANTHROPIC_AUTH_TOKEN ANTHROPIC_API_KEY\n"
}
