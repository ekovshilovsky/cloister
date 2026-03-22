package vmcli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/vmconfig"
)

// CheckResult holds the outcome of a single diagnostic check performed by the
// doctor command. Each check runs independently so that one failure does not
// prevent subsequent checks from executing.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "pass", "fail", "warn"
	Detail string `json:"detail"`
}

// String formats a CheckResult as a single line with a visual status indicator.
func (r CheckResult) String() string {
	var icon string
	switch r.Status {
	case "pass":
		icon = "\u2713" // check mark
	case "fail":
		icon = "\u2717" // ballot x
	case "warn":
		icon = "\u26A0" // warning sign
	default:
		icon = "?"
	}
	return fmt.Sprintf("%s %-20s %s", icon, r.Name, r.Detail)
}

// runCheckWithTimeout executes a diagnostic check function with a deadline.
// If the check does not complete within the specified timeout, a fail result
// is returned indicating the check timed out. The channel is buffered so the
// goroutine does not leak even when the timeout fires before fn returns.
func runCheckWithTimeout(name string, fn func() CheckResult, timeout time.Duration) CheckResult {
	ch := make(chan CheckResult, 1)
	go func() {
		result := fn()
		// Non-blocking send: if the receiver already timed out, the result
		// is discarded and the goroutine exits without blocking.
		select {
		case ch <- result:
		default:
		}
	}()

	select {
	case result := <-ch:
		return result
	case <-time.After(timeout):
		return CheckResult{
			Name:   name,
			Status: "fail",
			Detail: fmt.Sprintf("timed out after %s", timeout),
		}
	}
}

// checkClaudeInstalled verifies that the claude CLI is available on PATH and
// reports its version string.
func checkClaudeInstalled() CheckResult {
	out, err := exec.Command("claude", "--version").CombinedOutput()
	if err != nil {
		return CheckResult{
			Name:   "claude-cli",
			Status: "fail",
			Detail: "claude not found on PATH",
		}
	}
	version := strings.TrimSpace(string(out))
	return CheckResult{
		Name:   "claude-cli",
		Status: "pass",
		Detail: version,
	}
}

// checkClaudeMode reports whether Claude Code is configured for local (Ollama)
// or cloud (Anthropic) API access based on the ANTHROPIC_BASE_URL environment
// variable. The canonical local URL is "http://127.0.0.1:11434" as written by
// the claude-local command's env file.
func checkClaudeMode() CheckResult {
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" {
		return CheckResult{
			Name:   "claude-mode",
			Status: "pass",
			Detail: "cloud",
		}
	}
	if baseURL == "http://127.0.0.1:11434" {
		return CheckResult{
			Name:   "claude-mode",
			Status: "pass",
			Detail: "local (ollama)",
		}
	}
	return CheckResult{
		Name:   "claude-mode",
		Status: "pass",
		Detail: fmt.Sprintf("local (custom: %s)", baseURL),
	}
}

// checkConfigFile checks whether the default VM config file exists and is
// valid JSON.
func checkConfigFile() CheckResult {
	return checkConfigFileAt(DefaultConfigPath())
}

// checkConfigFileAt checks whether a config file exists at the given path and
// contains valid JSON that can be deserialized into a Config struct.
func checkConfigFileAt(path string) CheckResult {
	data, err := os.ReadFile(path)
	if err != nil {
		return CheckResult{
			Name:   "config-file",
			Status: "fail",
			Detail: fmt.Sprintf("not found: %s", path),
		}
	}

	var cfg vmconfig.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return CheckResult{
			Name:   "config-file",
			Status: "fail",
			Detail: fmt.Sprintf("malformed JSON: %v", err),
		}
	}

	return CheckResult{
		Name:   "config-file",
		Status: "pass",
		Detail: fmt.Sprintf("profile=%s", cfg.Profile),
	}
}

// checkTunnels probes all configured tunnels and reports aggregate connectivity.
func checkTunnels(cfg *vmconfig.Config) CheckResult {
	if cfg == nil || len(cfg.Tunnels) == 0 {
		return CheckResult{
			Name:   "tunnels",
			Status: "warn",
			Detail: "no tunnels configured",
		}
	}

	results := CheckTunnels(cfg.Tunnels, 500)
	connected := 0
	var downNames []string
	for _, r := range results {
		if r.Connected {
			connected++
		} else {
			downNames = append(downNames, r.Name)
		}
	}

	total := len(results)
	if connected == total {
		return CheckResult{
			Name:   "tunnels",
			Status: "pass",
			Detail: fmt.Sprintf("%d/%d connected", connected, total),
		}
	}
	if connected == 0 {
		return CheckResult{
			Name:   "tunnels",
			Status: "fail",
			Detail: fmt.Sprintf("0/%d connected (down: %s)", total, strings.Join(downNames, ", ")),
		}
	}
	return CheckResult{
		Name:   "tunnels",
		Status: "warn",
		Detail: fmt.Sprintf("%d/%d connected (down: %s)", connected, total, strings.Join(downNames, ", ")),
	}
}

// checkOpForwardTokenFile checks whether the op-forward refresh token file
// exists, which indicates a working 1Password tunnel authentication state.
func checkOpForwardTokenFile() CheckResult {
	home, _ := os.UserHomeDir()
	tokenPath := filepath.Join(home, ".cache", "op-forward", "refresh.token")
	return checkOpForwardTokenFileAt(tokenPath)
}

// checkOpForwardTokenFileAt checks whether a token file exists at the given path.
func checkOpForwardTokenFileAt(path string) CheckResult {
	info, err := os.Stat(path)
	if err != nil {
		return CheckResult{
			Name:   "op-forward-token",
			Status: "warn",
			Detail: "token file not found",
		}
	}
	if info.Size() == 0 {
		return CheckResult{
			Name:   "op-forward-token",
			Status: "warn",
			Detail: "token file is empty",
		}
	}
	return CheckResult{
		Name:   "op-forward-token",
		Status: "pass",
		Detail: "token file present",
	}
}

// RunDoctor executes all diagnostic checks and returns the collected results.
// The config parameter may be nil if config loading failed; checks that require
// config will be skipped or will report appropriately.
func RunDoctor(cfg *vmconfig.Config) []CheckResult {
	timeout := 5 * time.Second

	checks := []struct {
		name string
		fn   func() CheckResult
	}{
		{"claude-cli", checkClaudeInstalled},
		{"claude-mode", checkClaudeMode},
		{"config-file", checkConfigFile},
		{"tunnels", func() CheckResult { return checkTunnels(cfg) }},
		{"op-forward-token", checkOpForwardTokenFile},
	}

	// Append platform-specific system checks (swap, disk, memory) which are
	// populated via build-tag-guarded files.
	checks = append(checks, platformChecks()...)

	results := make([]CheckResult, 0, len(checks))
	for _, c := range checks {
		r := runCheckWithTimeout(c.name, c.fn, timeout)
		results = append(results, r)
	}

	return results
}

// FormatDoctorSummary returns a human-readable summary line for the doctor
// results, e.g. "5 passed, 1 warning, 0 failed".
func FormatDoctorSummary(results []CheckResult) string {
	var passed, warnings, failed int
	for _, r := range results {
		switch r.Status {
		case "pass":
			passed++
		case "warn":
			warnings++
		case "fail":
			failed++
		}
	}
	return fmt.Sprintf("%d passed, %d warnings, %d failed", passed, warnings, failed)
}
