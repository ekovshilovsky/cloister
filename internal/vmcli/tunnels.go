package vmcli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ekovshilovsky/cloister/internal/vmconfig"
)

// TunnelResult holds the health check result for a single tunnel.
type TunnelResult struct {
	Name      string `json:"name"`
	Port      int    `json:"port"`
	Connected bool   `json:"connected"`
	Detail    string `json:"detail,omitempty"`
}

// String formats a TunnelResult as a single line for display.
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
	return fmt.Sprintf("%-12s :%d  %s %s", r.Name, r.Port, icon, status)
}

// CheckTunnels probes each tunnel and returns results. timeoutMs controls
// the TCP dial timeout in milliseconds.
func CheckTunnels(tunnels []vmconfig.TunnelDef, timeoutMs int) []TunnelResult {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	results := make([]TunnelResult, 0, len(tunnels))

	for _, t := range tunnels {
		r := TunnelResult{
			Name:      t.Name,
			Port:      t.Port,
			Connected: ProbeTCP("127.0.0.1", t.Port, timeout),
		}

		// Enriched checks for specific well-known tunnels when connected.
		if r.Connected {
			switch t.Name {
			case "op-forward":
				r.Detail = checkOpForwardToken()
			case "ollama":
				r.Detail = checkOllamaModels()
			}
		}

		results = append(results, r)
	}

	return results
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
