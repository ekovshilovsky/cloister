package agent

import (
	"strings"
	"testing"
)

func TestOpenClawDefaults(t *testing.T) {
	cfg := OpenClawDefaults()
	if cfg.Type != "openclaw" {
		t.Errorf("Type = %q, want openclaw", cfg.Type)
	}
	if cfg.Image != "openclaw/openclaw:latest" {
		t.Errorf("Image = %q, want openclaw/openclaw:latest", cfg.Image)
	}
	if len(cfg.Ports) != 1 || cfg.Ports[0] != 3000 {
		t.Errorf("Ports = %v, want [3000]", cfg.Ports)
	}
	if cfg.AutoStart {
		t.Error("AutoStart should default to false (set on first start)")
	}
}

func TestOpenClawDockerArgs(t *testing.T) {
	cfg := OpenClawDefaults()
	args := DockerRunArgs("testprofile", cfg, "/home/user/.openclaw", "/home/user/code")
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--cap-drop ALL") {
		t.Error("should drop all capabilities")
	}
	if !strings.Contains(joined, "--cap-add SYS_ADMIN") {
		t.Error("should add SYS_ADMIN for Chromium")
	}
	if !strings.Contains(joined, "--shm-size=2g") {
		t.Error("should set shm-size for Chromium")
	}
	if !strings.Contains(joined, "--user 1000:1000") {
		t.Error("should run as non-root")
	}
	if !strings.Contains(joined, "127.0.0.1:3000:3000") {
		t.Error("should publish port to localhost only")
	}
}

func TestOpenClawDockerArgsWithEnv(t *testing.T) {
	cfg := OpenClawDefaults()
	cfg.Env = map[string]string{"API_KEY": "test"}
	args := DockerRunArgs("testprofile", cfg, "/home/user/.openclaw", "/home/user/code")
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "-e API_KEY=test") {
		t.Error("should inject env vars")
	}
}

func TestOpenClawStacks(t *testing.T) {
	stacks := OpenClawStacks()
	if len(stacks) != 1 || stacks[0] != "web" {
		t.Errorf("stacks = %v, want [web]", stacks)
	}
}
