package agent

import (
	"fmt"
	"sort"

	"github.com/ekovshilovsky/cloister/internal/config"
)

// OpenClawDefaults returns the default AgentConfig for an OpenClaw profile.
func OpenClawDefaults() *config.AgentConfig {
	return &config.AgentConfig{
		Type:  "openclaw",
		Image: "alpine/openclaw:latest",
		Ports: []int{3000},
	}
}

// OpenClawStacks returns the provisioning stacks required by OpenClaw.
func OpenClawStacks() []string {
	return []string{"web"}
}

// DockerRunArgs builds the `docker run` argument list for an agent container.
// agentDataDir is the VM-side path to the agent's data directory.
// workspaceDir is the VM-side workspace path.
func DockerRunArgs(profile string, cfg *config.AgentConfig, agentDataDir, workspaceDir string) []string {
	args := []string{
		"run", "-d",
		"--name", profile,
		"--cap-drop", "ALL",
		"--cap-add", "SYS_ADMIN",
		"--user", "1000:1000",
		"--shm-size=2g",
	}

	// Temp file volumes scoped to agent data directory
	args = append(args,
		"-v", fmt.Sprintf("%s/tmp:/tmp", agentDataDir),
		"-v", fmt.Sprintf("%s/tmp/browser-cache:/home/node/.cache", agentDataDir),
	)

	// Port publishing to VM localhost only
	for _, port := range cfg.Ports {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port))
	}

	// Data and workspace volumes
	args = append(args,
		"-v", fmt.Sprintf("%s:/home/node/.openclaw", agentDataDir),
		"-v", fmt.Sprintf("%s:/home/node/.openclaw/workspace", workspaceDir),
	)

	// Log rotation to cap disk usage from long-running containers
	args = append(args,
		"--log-opt", "max-size=10m",
		"--log-opt", "max-file=5",
	)

	// Environment variable overrides (sorted for deterministic output)
	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", fmt.Sprintf("%s=%s", k, cfg.Env[k]))
		}
	}

	args = append(args, cfg.Image)
	return args
}
