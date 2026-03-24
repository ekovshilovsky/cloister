package agent

import (
	"testing"
)

func TestBuildDockerExecCommand(t *testing.T) {
	cmd := buildDockerCommand("info")
	if cmd[0] != "docker" || cmd[1] != "info" {
		t.Errorf("unexpected command: %v", cmd)
	}
}

func TestBuildDockerStopCommand(t *testing.T) {
	cmd := buildDockerCommand("stop", "mycontainer")
	if len(cmd) != 3 || cmd[2] != "mycontainer" {
		t.Errorf("unexpected command: %v", cmd)
	}
}

func TestBuildDockerLogsCommand(t *testing.T) {
	cmd := buildDockerCommand("logs", "--tail", "100", "mycontainer")
	if len(cmd) != 5 {
		t.Errorf("expected 5 args, got %d: %v", len(cmd), cmd)
	}
}

func TestParseDockerInspectUptime(t *testing.T) {
	startedAt := "2026-03-22T10:00:00Z"
	uptime := parseUptime(startedAt)
	if uptime == "" {
		t.Error("uptime should not be empty for valid timestamp")
	}
}

func TestParseDockerInspectUptimeEmpty(t *testing.T) {
	uptime := parseUptime("")
	if uptime != "unknown" {
		t.Errorf("expected 'unknown' for empty input, got %q", uptime)
	}
}
