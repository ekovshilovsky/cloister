package vmcli

import (
	"strings"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/vmconfig"
)

func TestFormatStatus(t *testing.T) {
	cfg := &vmconfig.Config{
		Profile:     "work",
		Workspace:   "/Users/user/code/myapp",
		ClaudeLocal: true,
		Tunnels: []vmconfig.TunnelDef{
			{Name: "clipboard", Port: 18339},
			{Name: "ollama", Port: 11434},
		},
	}
	tunnelResults := []TunnelResult{
		{Name: "clipboard", Port: 18339, Connected: true},
		{Name: "ollama", Port: 11434, Connected: false},
	}

	output := FormatStatus(cfg, tunnelResults, 0)
	if !strings.Contains(output, "work") {
		t.Error("status should contain profile name")
	}
	if !strings.Contains(output, "clipboard") {
		t.Error("status should contain tunnel names")
	}
	if !strings.Contains(output, "/Users/user/code/myapp") {
		t.Error("status should contain workspace path")
	}
}

func TestFormatStatusBrief(t *testing.T) {
	cfg := &vmconfig.Config{
		Profile:     "work",
		Workspace:   "/Users/user/code/myapp",
		ClaudeLocal: false,
		Tunnels: []vmconfig.TunnelDef{
			{Name: "clipboard", Port: 18339},
		},
	}
	tunnelResults := []TunnelResult{
		{Name: "clipboard", Port: 18339, Connected: true},
	}

	output := FormatStatusBrief(cfg, tunnelResults, 3)
	if output == "" {
		t.Error("brief status should not be empty")
	}
	if !strings.Contains(output, "models: 3") {
		t.Error("brief status should contain model count")
	}
	// Brief output should be compact — typically one or two lines
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 5 {
		t.Errorf("brief status should be compact, got %d lines", len(lines))
	}
}

func TestModelCountFromTunnelResultsConnected(t *testing.T) {
	results := []TunnelResult{
		{Name: "clipboard", Port: 18339, Connected: true},
		{Name: "ollama", Port: 11434, Connected: true, Detail: "models: 4"},
	}
	if count := ModelCountFromTunnelResults(results); count != 4 {
		t.Errorf("expected 4 models, got %d", count)
	}
}

func TestModelCountFromTunnelResultsDisconnected(t *testing.T) {
	results := []TunnelResult{
		{Name: "clipboard", Port: 18339, Connected: true},
		{Name: "ollama", Port: 11434, Connected: false},
	}
	if count := ModelCountFromTunnelResults(results); count != 0 {
		t.Errorf("expected 0 models for disconnected ollama, got %d", count)
	}
}

func TestModelCountFromTunnelResultsNoOllama(t *testing.T) {
	results := []TunnelResult{
		{Name: "clipboard", Port: 18339, Connected: true},
	}
	if count := ModelCountFromTunnelResults(results); count != 0 {
		t.Errorf("expected 0 models with no ollama tunnel, got %d", count)
	}
}

func TestFormatStatusNoTunnels(t *testing.T) {
	cfg := &vmconfig.Config{
		Profile:   "minimal",
		Workspace: "/home/user",
	}
	output := FormatStatus(cfg, nil, 0)
	if !strings.Contains(output, "minimal") {
		t.Error("status should contain profile name even with no tunnels")
	}
}
