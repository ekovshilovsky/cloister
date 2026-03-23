package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// buildDockerCommand constructs a docker CLI command as a string slice.
func buildDockerCommand(args ...string) []string {
	return append([]string{"docker"}, args...)
}

// CheckDocker verifies the Docker daemon is operational inside the VM.
func CheckDocker(profile string) error {
	_, err := vm.SSHCommand(profile, "docker info > /dev/null 2>&1")
	if err != nil {
		return fmt.Errorf("Docker is not available in VM %q. Rebuild the profile to fix: cloister rebuild %s", profile, profile)
	}
	return nil
}

// StartContainer runs the agent's Docker container inside the VM.
func StartContainer(profile string, agentCfg *config.AgentConfig, agentDataDir, workspaceDir string) (string, error) {
	args := DockerRunArgs(profile, agentCfg, agentDataDir, workspaceDir)
	cmd := strings.Join(args, " ")

	// Ensure tmp directories exist inside the VM
	mkdirCmd := fmt.Sprintf("mkdir -p %s/tmp/browser-cache", agentDataDir)
	if _, err := vm.SSHCommand(profile, mkdirCmd); err != nil {
		return "", fmt.Errorf("creating agent tmp directories: %w", err)
	}

	out, err := vm.SSHCommand(profile, "docker "+cmd)
	if err != nil {
		return "", fmt.Errorf("starting agent container: %w\n%s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// StopContainer stops and removes the agent's Docker container inside the VM.
func StopContainer(profile, containerID string) error {
	if _, err := vm.SSHCommand(profile, fmt.Sprintf("docker stop -t 10 %s", containerID)); err != nil {
		fmt.Printf("Warning: stop may have failed: %v\n", err)
	}
	if _, err := vm.SSHCommand(profile, fmt.Sprintf("docker rm -f %s", containerID)); err != nil {
		return fmt.Errorf("removing container: %w", err)
	}
	return nil
}

// ContainerStatus holds the runtime state of an agent container.
type ContainerStatus struct {
	Profile   string `json:"profile"`
	State     string `json:"state"`
	Uptime    string `json:"uptime"`
	Image     string `json:"image"`
	Ports     []int  `json:"ports"`
	AutoStart bool   `json:"auto_start"`
}

// InspectContainer queries the running container's state inside the VM.
func InspectContainer(profile, containerID string) (*ContainerStatus, error) {
	out, err := vm.SSHCommand(profile, fmt.Sprintf(
		`docker inspect --format '{"state":"{{.State.Status}}","started":"{{.State.StartedAt}}","image":"{{.Config.Image}}"}' %s`,
		containerID,
	))
	if err != nil {
		return nil, fmt.Errorf("inspecting container: %w", err)
	}

	var info struct {
		State   string `json:"state"`
		Started string `json:"started"`
		Image   string `json:"image"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &info); err != nil {
		return nil, fmt.Errorf("parsing container info: %w", err)
	}

	return &ContainerStatus{
		Profile: profile,
		State:   info.State,
		Uptime:  parseUptime(info.Started),
		Image:   info.Image,
	}, nil
}

// ContainerLogs streams or tails container logs from the VM.
// When follow is true, the command is run interactively (attached to stdout/stderr).
func ContainerLogs(profile, containerID string, follow bool) error {
	if follow {
		return vm.SSHInteractive(profile, fmt.Sprintf("docker logs -f %s", containerID))
	}
	out, err := vm.SSHCommand(profile, fmt.Sprintf("docker logs --tail 100 %s", containerID))
	if err != nil {
		return fmt.Errorf("reading container logs: %w", err)
	}
	fmt.Print(out)
	return nil
}

// parseUptime converts an ISO 8601 timestamp to a human-readable duration.
func parseUptime(startedAt string) string {
	if startedAt == "" {
		return "unknown"
	}
	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return "unknown"
	}
	d := time.Since(t)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if hours < 24 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	days := hours / 24
	hours = hours % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}
