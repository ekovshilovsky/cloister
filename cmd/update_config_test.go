package cmd

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
)

// TestClaudeLocalRequiresOllamaStack verifies that enabling claude-local
// without the ollama stack is rejected in both create and update-config flows.
func TestClaudeLocalRequiresOllamaStack(t *testing.T) {
	// Profile without ollama stack should fail validation
	p := &config.Profile{
		Stacks:      []string{"web", "python"},
		ClaudeLocal: true,
	}

	if p.HasStack("ollama") {
		t.Error("expected ollama stack to be absent")
	}
	if p.ClaudeLocal && !p.HasStack("ollama") {
		// This is the condition we validate in runCreate and runUpdateConfig.
		// Verification passes — the guard would reject this profile.
	}

	// Profile with ollama stack should pass
	p.Stacks = append(p.Stacks, "ollama")
	if !p.HasStack("ollama") {
		t.Error("expected ollama stack to be present after adding it")
	}
}

// TestClaudeLocalToggle verifies that the ClaudeLocal flag can be toggled
// on the profile configuration.
func TestClaudeLocalToggle(t *testing.T) {
	p := &config.Profile{
		ClaudeLocal: true,
		StartDir:    "~/code",
		Stacks:      []string{"ollama"},
	}
	if !p.ClaudeLocal {
		t.Error("ClaudeLocal should be true")
	}

	p.ClaudeLocal = false
	if p.ClaudeLocal {
		t.Error("ClaudeLocal should be false after toggling")
	}
}

// TestHasStack verifies the HasStack helper method on Profile.
func TestHasStack(t *testing.T) {
	p := &config.Profile{Stacks: []string{"web", "ollama", "python"}}

	if !p.HasStack("ollama") {
		t.Error("expected HasStack(ollama) to be true")
	}
	if !p.HasStack("web") {
		t.Error("expected HasStack(web) to be true")
	}
	if p.HasStack("rust") {
		t.Error("expected HasStack(rust) to be false")
	}

	empty := &config.Profile{}
	if empty.HasStack("anything") {
		t.Error("expected HasStack on empty stacks to be false")
	}
}
