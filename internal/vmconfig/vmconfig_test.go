package vmconfig

import (
	"encoding/json"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	cfg := Config{
		Profile: "work",
		Tunnels: []TunnelDef{
			{Name: "clipboard", Port: 18339},
			{Name: "op-forward", Port: 18340, Health: "http://127.0.0.1:18340/health"},
		},
		Workspace:   "/Users/user/code/myapp",
		ClaudeLocal: true,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var loaded Config
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if loaded.Profile != "work" {
		t.Errorf("Profile = %q, want %q", loaded.Profile, "work")
	}
	if len(loaded.Tunnels) != 2 {
		t.Fatalf("Tunnels len = %d, want 2", len(loaded.Tunnels))
	}
	if loaded.Tunnels[1].Health != "http://127.0.0.1:18340/health" {
		t.Errorf("Health = %q, want health URL", loaded.Tunnels[1].Health)
	}
	if !loaded.ClaudeLocal {
		t.Error("ClaudeLocal should be true")
	}
}

func TestConfigOmitsEmptyHealth(t *testing.T) {
	cfg := Config{
		Profile: "test",
		Tunnels: []TunnelDef{{Name: "audio", Port: 4713}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tunnels := m["tunnels"].([]interface{})
	tunnel := tunnels[0].(map[string]interface{})
	if _, has := tunnel["health"]; has {
		t.Error("empty health should be omitted from JSON output")
	}
}
