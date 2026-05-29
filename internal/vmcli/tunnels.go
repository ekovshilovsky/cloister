package vmcli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/vmconfig"
)

// TunnelResult holds the health check result for a single tunnel.
type TunnelResult struct {
	Name      string `json:"name"`
	Port      int    `json:"port"`
	Socket    string `json:"socket,omitempty"`
	Connected bool   `json:"connected"`
	Detail    string `json:"detail,omitempty"`
}

// String formats a TunnelResult as a single line for display. Socket tunnels
// render their resolved socket path in place of the TCP port column.
func (r TunnelResult) String() string {
	icon := "✗"
	status := "not connected"
	if r.Connected {
		icon = "✓"
		status = "connected"
	}
	if r.Detail != "" {
		status += " (" + r.Detail + ")"
	}
	if r.Socket != "" {
		return fmt.Sprintf("%-12s %s  %s %s", r.Name, r.Socket, icon, status)
	}
	return fmt.Sprintf("%-12s :%d  %s %s", r.Name, r.Port, icon, status)
}

// CheckTunnels probes each tunnel and returns results. timeoutMs controls
// the TCP dial timeout in milliseconds. Socket tunnels (Socket field set)
// are checked by stat-ing the resolved socket path; the literal substring
// "$UID" in the path is substituted against the current process UID before
// probing.
func CheckTunnels(tunnels []vmconfig.TunnelDef, timeoutMs int) []TunnelResult {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	uidStr := strconv.Itoa(os.Getuid())
	results := make([]TunnelResult, 0, len(tunnels))

	for _, t := range tunnels {
		r := TunnelResult{
			Name: t.Name,
			Port: t.Port,
		}

		if t.Socket != "" {
			// Socket tunnel: substitute $UID then stat the path. There is no
			// TCP port to probe; the connection is "live" iff the path is a
			// real Unix-domain socket on the VM filesystem.
			resolved := strings.ReplaceAll(t.Socket, "$UID", uidStr)
			r.Socket = resolved
			r.Connected = isUnixSocket(resolved)
		} else {
			r.Connected = ProbeTCP("127.0.0.1", t.Port, timeout)
		}

		// Enriched checks for specific well-known tunnels when connected.
		if r.Connected {
			switch t.Name {
			case "op-forward":
				r.Detail = checkOpForwardToken()
			case "ollama":
				r.Detail = checkOllamaModels()
			case "clipboard":
				// TCP reachability alone is insufficient to declare the
				// clipboard tunnel functional: the in-VM cc-clip CLI binary
				// is what actually translates user paste operations into
				// daemon requests over the tunnel. Without it the green
				// check would be a lie — the port is forwarded but every
				// paste fails with "cc-clip: command not found". Verify the
				// binary is on PATH and downgrade the result if not, so the
				// status output matches end-to-end functionality.
				if _, err := exec.LookPath("cc-clip"); err != nil {
					r.Connected = false
					r.Detail = "cc-clip binary missing — run 'cloister repair' to reinstall"
				}
			}
		}

		results = append(results, r)
	}

	return results
}

// isUnixSocket returns true when path exists and is a Unix-domain socket.
// Used by CheckTunnels to decide connectivity for socket-style tunnels
// (e.g., gpg-forward) where there is no TCP port to probe.
func isUnixSocket(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0
}

// checkOpForwardToken checks if the op-forward refresh token file exists and
// is non-empty, confirming that the 1Password SSH agent tunnel is authenticated.
func checkOpForwardToken() string {
	home, _ := os.UserHomeDir()
	tokenPath := filepath.Join(home, ".cache", "op-forward", "refresh.token")
	info, err := os.Stat(tokenPath)
	if err != nil || info.Size() == 0 {
		return "token: missing"
	}
	return "token: present"
}

// checkOllamaModels queries the Ollama API and returns the number of installed
// models as a human-readable detail string.
func checkOllamaModels() string {
	models, err := FetchOllamaModels()
	if err != nil {
		return "models: error"
	}
	return fmt.Sprintf("models: %d", len(models))
}
