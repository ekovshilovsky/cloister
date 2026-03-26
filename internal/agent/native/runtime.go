package native

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/agent"
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// Runtime manages OpenClaw agents running natively in macOS Lume VMs.
// Unlike DockerRuntime which manages Docker containers, NativeRuntime
// interacts with the OpenClaw daemon process directly via SSH.
type Runtime struct{}

// Start launches the OpenClaw gateway daemon inside the Lume VM for the given
// profile. It is idempotent: if the daemon is already running, Start returns
// nil without issuing a second start command.
func (r *Runtime) Start(profile string, cfg *config.AgentConfig, dataDir, workspaceDir string, backend vm.Backend) error {
	// Avoid double-starting a daemon that is already running.
	if r.IsRunning(profile, backend) {
		return nil
	}
	// Start the OpenClaw gateway daemon in background mode.
	_, err := backend.SSHCommand(profile, "openclaw gateway start --daemon")
	if err != nil {
		return fmt.Errorf("starting OpenClaw gateway: %w", err)
	}
	return nil
}

// Stop sends a graceful shutdown signal to the OpenClaw gateway daemon running
// inside the Lume VM for the given profile.
func (r *Runtime) Stop(profile string, backend vm.Backend) error {
	_, err := backend.SSHCommand(profile, "openclaw gateway stop")
	if err != nil {
		return fmt.Errorf("stopping OpenClaw gateway: %w", err)
	}
	return nil
}

// Status queries the OpenClaw gateway daemon status via SSH and returns a
// normalised AgentStatus. If the daemon is unreachable or returns malformed
// JSON, a safe degraded status is returned rather than an error, so that
// callers can always display a meaningful state to the user.
func (r *Runtime) Status(profile string, backend vm.Backend) (*agent.AgentStatus, error) {
	out, err := backend.SSHCommand(profile, "openclaw gateway status --json 2>/dev/null || echo '{}'")
	if err != nil {
		return &agent.AgentStatus{
			Profile: profile,
			State:   "stopped",
		}, nil
	}

	// Parse the OpenClaw daemon status payload.
	var status struct {
		Running bool   `json:"running"`
		Version string `json:"version"`
		Uptime  int64  `json:"uptime_seconds"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &status); err != nil {
		return &agent.AgentStatus{
			Profile: profile,
			State:   "unknown",
		}, nil
	}

	state := "stopped"
	uptime := "-"
	if status.Running {
		state = "running"
		d := time.Duration(status.Uptime) * time.Second
		uptime = formatUptime(d)
	}

	return &agent.AgentStatus{
		Profile: profile,
		State:   state,
		Uptime:  uptime,
		Image:   fmt.Sprintf("openclaw %s", status.Version),
	}, nil
}

// Logs streams or tails the OpenClaw gateway log output from the Lume VM.
// When follow is true the command is run in an interactive SSH session so
// that output is streamed to the caller's terminal in real time.
func (r *Runtime) Logs(profile string, follow bool, backend vm.Backend) error {
	if follow {
		return backend.SSHInteractive(profile, "openclaw gateway logs --follow")
	}
	out, err := backend.SSHCommand(profile, "openclaw gateway logs --tail 100")
	if err != nil {
		return fmt.Errorf("reading OpenClaw logs: %w", err)
	}
	fmt.Print(out)
	return nil
}

// IsRunning probes the Lume VM to determine whether the OpenClaw gateway
// process is currently alive. It uses pgrep rather than the daemon's own
// status command so that it works even when the daemon's IPC socket is
// unavailable.
func (r *Runtime) IsRunning(profile string, backend vm.Backend) bool {
	out, err := backend.SSHCommand(profile, "pgrep -f 'openclaw gateway' >/dev/null 2>&1 && echo running || echo stopped")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "running"
}

// formatUptime converts a duration into a concise human-readable string
// (e.g. "42s", "15m", "3h 22m", "2d 5h").
func formatUptime(d time.Duration) string {
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
