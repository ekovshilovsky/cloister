package agent

import (
	"encoding/json"
	"fmt"
	"os"
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

// StartContainer runs the agent's container(s) inside the VM. For OpenClaw
// profiles, it deploys a docker-compose.yml with dual services (gateway + CLI)
// matching the official upstream architecture. For other agent types, it falls
// back to a single docker run.
func StartContainer(profile string, agentCfg *config.AgentConfig, agentDataDir, workspaceDir string) (string, error) {
	// Ensure tmp directories exist inside the VM
	mkdirCmd := fmt.Sprintf("mkdir -p %s/tmp/browser-cache", agentDataDir)
	if _, err := vm.SSHCommand(profile, mkdirCmd); err != nil {
		return "", fmt.Errorf("creating agent tmp directories: %w", err)
	}

	if agentCfg.Type == "openclaw" {
		return startWithCompose(profile, agentCfg, agentDataDir, workspaceDir)
	}
	return startWithDockerRun(profile, agentCfg, agentDataDir, workspaceDir)
}

// startWithCompose deploys the dual-service docker-compose stack for OpenClaw.
// The compose file is generated on the host and mounted read-only into the VM
// so the agent cannot tamper with its own container configuration. The compose
// directory is separate from the agent data directory which remains writable.
func startWithCompose(profile string, agentCfg *config.AgentConfig, agentDataDir, workspaceDir string) (string, error) {
	// The compose file lives in a sibling directory mounted read-only.
	// The host path is resolved by the caller and mounted via BuildMounts.
	// Inside the VM it appears at the same path (virtiofs passthrough).
	composeDir := ComposeDir(profile, agentCfg.Type)
	composePath := fmt.Sprintf("%s/docker-compose.yml", composeDir)

	// Verify the compose file is accessible inside the VM (it was mounted
	// from the host by the VM start flow).
	checkCmd := fmt.Sprintf("test -f %s && echo ok || echo missing", composePath)
	checkOut, _ := vm.SSHCommand(profile, checkCmd)
	if strings.TrimSpace(checkOut) != "ok" {
		return "", fmt.Errorf("docker-compose.yml not found at %s inside VM — the compose directory may not be mounted. Rebuild the profile", composePath)
	}

	// Start the compose stack from the compose directory
	upCmd := fmt.Sprintf("cd %s && docker compose up -d", composeDir)
	out, err := vm.SSHCommand(profile, upCmd)
	if err != nil {
		return "", fmt.Errorf("starting compose stack: %w\n%s", err, out)
	}

	// Return the gateway container ID for state tracking
	idCmd := fmt.Sprintf("docker inspect --format '{{.Id}}' %s-gateway", profile)
	idOut, err := vm.SSHCommand(profile, idCmd)
	if err != nil {
		return "", fmt.Errorf("reading gateway container ID: %w", err)
	}
	return strings.TrimSpace(idOut), nil
}

// WriteComposeFile generates the docker-compose.yml on the host filesystem.
// Called during profile creation and agent start to ensure the compose file
// is current. The file is mounted read-only into the VM.
func WriteComposeFile(profile string, agentCfg *config.AgentConfig, agentDataDir, workspaceDir string) error {
	composeDir := ComposeDir(profile, agentCfg.Type)
	if err := os.MkdirAll(composeDir, 0o700); err != nil {
		return fmt.Errorf("creating compose directory: %w", err)
	}
	content := ComposeYAML(profile, agentCfg, agentDataDir, workspaceDir)
	path := fmt.Sprintf("%s/docker-compose.yml", composeDir)
	return os.WriteFile(path, []byte(content), 0o600)
}

// ComposeDir returns the host-side directory for the agent's docker-compose.yml.
// This is separate from the agent data directory so it can be mounted read-only.
func ComposeDir(profile, agentType string) string {
	home, _ := os.UserHomeDir()
	return fmt.Sprintf("%s/.cloister/agents/%s/compose", home, profile)
}

// startWithDockerRun uses a single docker run for non-OpenClaw agent types.
func startWithDockerRun(profile string, agentCfg *config.AgentConfig, agentDataDir, workspaceDir string) (string, error) {
	args := DockerRunArgs(profile, agentCfg, agentDataDir, workspaceDir)
	cmd := strings.Join(args, " ")

	out, err := vm.SSHCommand(profile, "docker "+cmd)
	if err != nil {
		return "", fmt.Errorf("starting agent container: %w\n%s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// StopContainer stops and removes the agent's container(s) inside the VM.
// For OpenClaw, it uses docker compose down; for others, docker stop + rm.
func StopContainer(profile string, containerID string) error {
	return StopContainerWithType(profile, containerID, "")
}

// StopContainerWithType stops containers, using compose for OpenClaw profiles.
func StopContainerWithType(profile, containerID, agentType string) error {
	if agentType == "openclaw" {
		return stopWithCompose(profile)
	}
	return stopWithDockerRm(profile, containerID)
}

// stopWithCompose tears down the full compose stack.
func stopWithCompose(profile string) error {
	composeDir := ComposeDir(profile, "openclaw")
	cmd := fmt.Sprintf("cd %s && docker compose down --timeout 10", composeDir)
	if _, err := vm.SSHCommand(profile, cmd); err != nil {
		// Fallback: force remove by container name
		vm.SSHCommand(profile, fmt.Sprintf("docker rm -f %s-gateway %s-cli 2>/dev/null", profile, profile))
	}
	return nil
}

// stopWithDockerRm stops a single container with docker stop + rm.
func stopWithDockerRm(profile, containerID string) error {
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
// For OpenClaw, it inspects the gateway container.
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
// For OpenClaw, it targets the gateway container.
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
