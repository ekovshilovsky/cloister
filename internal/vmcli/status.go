package vmcli

import (
	"fmt"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/vmconfig"
)

// StatusData is the structured representation of VM environment status, used
// for both human-readable formatting and JSON serialization.
type StatusData struct {
	Profile    string         `json:"profile"`
	Claude     string         `json:"claude"`
	Tunnels    []TunnelResult `json:"tunnels"`
	ModelCount int            `json:"model_count"`
	Workspace  string         `json:"workspace"`
}

// BuildStatusData assembles a StatusData from config, tunnel probe results,
// and an optional model count (pass 0 if the ollama tunnel is unavailable).
func BuildStatusData(cfg *vmconfig.Config, tunnelResults []TunnelResult, modelCount int) StatusData {
	claudeMode := "cloud"
	if cfg.ClaudeLocal {
		claudeMode = "local (via Ollama)"
	}

	return StatusData{
		Profile:    cfg.Profile,
		Claude:     claudeMode,
		Tunnels:    tunnelResults,
		ModelCount: modelCount,
		Workspace:  cfg.Workspace,
	}
}

// FormatStatus renders a multi-line human-readable status overview, e.g.:
//
//	Profile:   work
//	Claude:    local (via Ollama)
//	Tunnels:   clipboard ✓  ollama ✗
//	Models:    unavailable
//	Workspace: /Users/user/code/myapp
//
// modelCount is the number of Ollama models; pass 0 when the ollama tunnel is
// down or the count is unknown.
func FormatStatus(cfg *vmconfig.Config, tunnelResults []TunnelResult, modelCount int) string {
	d := BuildStatusData(cfg, tunnelResults, modelCount)

	var sb strings.Builder

	fmt.Fprintf(&sb, "Profile:   %s\n", d.Profile)
	fmt.Fprintf(&sb, "Claude:    %s\n", d.Claude)

	// Render each tunnel with a ✓/✗ inline indicator.
	if len(d.Tunnels) > 0 {
		var parts []string
		for _, t := range d.Tunnels {
			icon := "✗"
			if t.Connected {
				icon = "✓"
			}
			parts = append(parts, fmt.Sprintf("%s %s", t.Name, icon))
		}
		fmt.Fprintf(&sb, "Tunnels:   %s\n", strings.Join(parts, "  "))
	} else {
		fmt.Fprintf(&sb, "Tunnels:   (none configured)\n")
	}

	// Omit the Models line when the count is unavailable to avoid clutter.
	if modelCount > 0 {
		fmt.Fprintf(&sb, "Models:    %d available\n", modelCount)
	} else {
		fmt.Fprintf(&sb, "Models:    unavailable\n")
	}

	fmt.Fprintf(&sb, "Workspace: %s\n", d.Workspace)

	return sb.String()
}

// ModelCountFromTunnelResults extracts the Ollama model count from tunnel
// enrichment results. Returns 0 if the ollama tunnel is not connected or
// the count could not be determined. This avoids a separate HTTP call since
// CheckTunnels already queries the Ollama API when the tunnel is up.
func ModelCountFromTunnelResults(results []TunnelResult) int {
	for _, r := range results {
		if r.Name == "ollama" && r.Connected && strings.HasPrefix(r.Detail, "models: ") {
			var count int
			fmt.Sscanf(r.Detail, "models: %d", &count)
			return count
		}
	}
	return 0
}

// FormatStatusBrief renders a compact single-line status summary intended for
// use as a shell login banner where latency and screen space are constrained.
// Example output:
//
//	cloister: innolumi | claude: local | tunnels: 3/4 | models: 4
func FormatStatusBrief(cfg *vmconfig.Config, tunnelResults []TunnelResult, modelCount int) string {
	claudeMode := "cloud"
	if cfg.ClaudeLocal {
		claudeMode = "local"
	}

	connected := 0
	for _, t := range tunnelResults {
		if t.Connected {
			connected++
		}
	}
	total := len(tunnelResults)

	return fmt.Sprintf("cloister: %s | claude: %s | tunnels: %d/%d | models: %d\n",
		cfg.Profile, claudeMode, connected, total, modelCount)
}
