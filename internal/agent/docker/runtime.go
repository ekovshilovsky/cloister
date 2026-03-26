package docker

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/agent"
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// DockerRuntime manages agent containers via Docker commands executed over SSH
// inside a VM. It implements agent.Runtime for profiles that run their agent
// workloads as Docker containers (both single-container and compose-based).
type DockerRuntime struct{}

// CheckDocker verifies that the Docker daemon is operational inside the VM
// managed by the given backend. Returns an actionable error when Docker is
// not reachable, directing the user to rebuild the profile.
func (r *DockerRuntime) CheckDocker(profile string, backend vm.Backend) error {
	_, err := backend.SSHCommand(profile, "docker info > /dev/null 2>&1")
	if err != nil {
		return fmt.Errorf("Docker is not available in VM %q. Rebuild the profile to fix: cloister rebuild %s", profile, profile)
	}
	return nil
}

// Start launches the agent's container(s) inside the VM and persists the
// container ID to the agent state directory. For OpenClaw profiles it deploys
// a docker-compose stack; for other agent types it falls back to docker run.
func (r *DockerRuntime) Start(profile string, cfg *config.AgentConfig, dataDir, workspaceDir string, backend vm.Backend) error {
	containerID, err := r.StartContainer(profile, cfg, dataDir, workspaceDir, backend)
	if err != nil {
		return err
	}

	// Persist the container ID so that Stop, Status, and Logs can locate it.
	stateDir, err := agent.StateDir()
	if err != nil {
		return fmt.Errorf("resolving state directory: %w", err)
	}
	return agent.WriteContainerID(stateDir, profile, containerID)
}

// StartContainer launches the agent's container(s) and returns the primary
// container ID without persisting it. This is useful for callers that manage
// state persistence themselves (e.g., backward-compatibility wrappers).
func (r *DockerRuntime) StartContainer(profile string, cfg *config.AgentConfig, dataDir, workspaceDir string, backend vm.Backend) (string, error) {
	// Ensure temporary directories exist inside the VM for browser caches and
	// other transient state that should not persist across container restarts.
	mkdirCmd := fmt.Sprintf("mkdir -p %s/tmp/browser-cache", dataDir)
	if _, err := backend.SSHCommand(profile, mkdirCmd); err != nil {
		return "", fmt.Errorf("creating agent tmp directories: %w", err)
	}

	if cfg.Type == "openclaw" {
		return r.startWithCompose(profile, cfg, dataDir, workspaceDir, backend)
	}
	return r.startWithDockerRun(profile, cfg, dataDir, workspaceDir, backend)
}

// startWithCompose deploys the dual-service docker-compose stack for OpenClaw.
// The compose file is generated on the host and mounted read-only into the VM
// so the agent cannot tamper with its own container configuration.
func (r *DockerRuntime) startWithCompose(profile string, cfg *config.AgentConfig, dataDir, workspaceDir string, backend vm.Backend) (string, error) {
	composeDir := agent.ComposeDir(profile, cfg.Type)
	composePath := fmt.Sprintf("%s/docker-compose.yml", composeDir)

	// Verify the compose file is accessible inside the VM (it was mounted
	// from the host by the VM start flow).
	checkCmd := fmt.Sprintf("test -f %s && echo ok || echo missing", composePath)
	checkOut, _ := backend.SSHCommand(profile, checkCmd)
	if strings.TrimSpace(checkOut) != "ok" {
		return "", fmt.Errorf("docker-compose.yml not found at %s inside VM — the compose directory may not be mounted. Rebuild the profile", composePath)
	}

	// Start the compose stack from the compose directory.
	upCmd := fmt.Sprintf("cd %s && docker compose up -d", composeDir)
	out, err := backend.SSHCommand(profile, upCmd)
	if err != nil {
		return "", fmt.Errorf("starting compose stack: %w\n%s", err, out)
	}

	// Return the gateway container ID for state tracking.
	idCmd := fmt.Sprintf("docker inspect --format '{{.Id}}' %s-gateway", profile)
	idOut, err := backend.SSHCommand(profile, idCmd)
	if err != nil {
		return "", fmt.Errorf("reading gateway container ID: %w", err)
	}
	return strings.TrimSpace(idOut), nil
}

// startWithDockerRun uses a single docker run for non-OpenClaw agent types.
func (r *DockerRuntime) startWithDockerRun(profile string, cfg *config.AgentConfig, dataDir, workspaceDir string, backend vm.Backend) (string, error) {
	args := DockerRunArgs(profile, cfg, dataDir, workspaceDir)
	cmd := strings.Join(args, " ")

	out, err := backend.SSHCommand(profile, "docker "+cmd)
	if err != nil {
		return "", fmt.Errorf("starting agent container: %w\n%s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// Stop tears down the agent's container(s) inside the VM. It reads the
// container ID from the persisted state and delegates to the appropriate
// teardown strategy.
func (r *DockerRuntime) Stop(profile string, backend vm.Backend) error {
	return r.StopWithType(profile, "", backend)
}

// StopWithType stops containers, using compose for OpenClaw profiles and
// docker stop + rm for other agent types.
func (r *DockerRuntime) StopWithType(profile, agentType string, backend vm.Backend) error {
	if agentType == "openclaw" {
		return r.stopWithCompose(profile, backend)
	}

	// Read the container ID from state for non-compose teardown.
	stateDir, err := agent.StateDir()
	if err != nil {
		return fmt.Errorf("resolving state directory: %w", err)
	}
	containerID, err := agent.ReadContainerID(stateDir, profile)
	if err != nil {
		return fmt.Errorf("no running agent for profile %q", profile)
	}
	return r.stopWithDockerRm(profile, containerID, backend)
}

// StopWithContainerID stops a single container by its ID. This preserves the
// original calling convention used by the backward-compatibility wrappers in
// the agent package.
func (r *DockerRuntime) StopWithContainerID(profile, containerID, agentType string, backend vm.Backend) error {
	if agentType == "openclaw" {
		return r.stopWithCompose(profile, backend)
	}
	return r.stopWithDockerRm(profile, containerID, backend)
}

// stopWithCompose tears down the full compose stack.
func (r *DockerRuntime) stopWithCompose(profile string, backend vm.Backend) error {
	composeDir := agent.ComposeDir(profile, "openclaw")
	cmd := fmt.Sprintf("cd %s && docker compose down --timeout 10", composeDir)
	if _, err := backend.SSHCommand(profile, cmd); err != nil {
		// Fallback: force remove containers by name if compose down fails.
		backend.SSHCommand(profile, fmt.Sprintf("docker rm -f %s-gateway %s-cli 2>/dev/null", profile, profile)) //nolint:errcheck
	}
	return nil
}

// stopWithDockerRm stops a single container with docker stop + rm.
func (r *DockerRuntime) stopWithDockerRm(profile, containerID string, backend vm.Backend) error {
	if _, err := backend.SSHCommand(profile, fmt.Sprintf("docker stop -t 10 %s", containerID)); err != nil {
		fmt.Printf("Warning: stop may have failed: %v\n", err)
	}
	if _, err := backend.SSHCommand(profile, fmt.Sprintf("docker rm -f %s", containerID)); err != nil {
		return fmt.Errorf("removing container: %w", err)
	}
	return nil
}

// Status queries the running container's state inside the VM and returns it
// as an AgentStatus. It reads the container ID from the persisted state.
func (r *DockerRuntime) Status(profile string, backend vm.Backend) (*agent.AgentStatus, error) {
	stateDir, err := agent.StateDir()
	if err != nil {
		return nil, fmt.Errorf("resolving state directory: %w", err)
	}
	containerID, err := agent.ReadContainerID(stateDir, profile)
	if err != nil {
		return nil, fmt.Errorf("no running agent for profile %q", profile)
	}
	return r.StatusByContainerID(profile, containerID, backend)
}

// StatusByContainerID queries a specific container's state inside the VM.
// This is used by both the Status method and callers that already have a
// container ID available (e.g., backward-compatibility wrappers).
func (r *DockerRuntime) StatusByContainerID(profile, containerID string, backend vm.Backend) (*agent.AgentStatus, error) {
	out, err := backend.SSHCommand(profile, fmt.Sprintf(
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

	return &agent.AgentStatus{
		Profile: profile,
		State:   info.State,
		Uptime:  parseUptime(info.Started),
		Image:   info.Image,
	}, nil
}

// Logs streams or tails container logs from the VM. For follow mode it uses
// an interactive SSH session to stream output; otherwise it tails the last
// 100 lines. It reads the container ID from the persisted state.
func (r *DockerRuntime) Logs(profile string, follow bool, backend vm.Backend) error {
	stateDir, err := agent.StateDir()
	if err != nil {
		return fmt.Errorf("resolving state directory: %w", err)
	}
	containerID, err := agent.ReadContainerID(stateDir, profile)
	if err != nil {
		return fmt.Errorf("no running agent for profile %q", profile)
	}
	return r.LogsByContainerID(profile, containerID, follow, backend)
}

// LogsByContainerID streams or tails logs for a specific container. This is
// used by both the Logs method and callers that already have a container ID.
func (r *DockerRuntime) LogsByContainerID(profile, containerID string, follow bool, backend vm.Backend) error {
	if follow {
		return backend.SSHInteractive(profile, fmt.Sprintf("docker logs -f %s", containerID))
	}
	out, err := backend.SSHCommand(profile, fmt.Sprintf("docker logs --tail 100 %s", containerID))
	if err != nil {
		return fmt.Errorf("reading container logs: %w", err)
	}
	fmt.Print(out)
	return nil
}

// IsRunning reports whether the agent's primary container is alive inside the
// VM by checking the container's running state via docker inspect.
func (r *DockerRuntime) IsRunning(profile string, backend vm.Backend) bool {
	stateDir, err := agent.StateDir()
	if err != nil {
		return false
	}
	containerID, err := agent.ReadContainerID(stateDir, profile)
	if err != nil {
		return false
	}
	out, err := backend.SSHCommand(profile, fmt.Sprintf(
		"docker inspect --format '{{.State.Running}}' %s 2>/dev/null", containerID,
	))
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
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
