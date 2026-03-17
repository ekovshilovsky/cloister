// Package memory provides budget management for VM memory allocation.
// It evaluates whether starting a new profile would cause total running VM
// memory to exceed the configured or auto-calculated budget, and surfaces
// actionable suggestions to the caller.
package memory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
)

// CheckResult holds the outcome of a budget evaluation and all data needed
// to generate user-facing messages.
type CheckResult struct {
	// Exceeded is true when starting NewProfile would push total consumption
	// above the available budget.
	Exceeded bool

	// Used is the total gigabytes currently consumed by all running VMs.
	Used int

	// Budget is the effective memory budget in gigabytes for this evaluation.
	Budget int

	// NewProfile is the name of the profile being started.
	NewProfile string

	// NewMemory is the gigabytes that NewProfile requires.
	NewMemory int

	// Candidates lists running VMs eligible for eviction, ordered by
	// descending idle duration so that the longest-idle VM appears first.
	Candidates []Candidate
}

// Candidate represents a running VM that could be stopped to free memory.
type Candidate struct {
	// Name is the cloister profile name.
	Name string

	// Memory is the gigabytes allocated to this VM.
	Memory int

	// Idle is how long it has been since the profile was last entered.
	// Zero means no last_entry timestamp was found.
	Idle time.Duration
}

// Check evaluates whether starting newProfile would exceed the memory budget.
// running is a map of profile name → true for currently running VMs.
// stateDir is the directory containing <profile>.last_entry idle timestamp
// files; pass an empty string to use the default (~/.cloister/state).
// GetSystemRAM is called to derive the auto-budget when cfg.MemoryBudget is zero.
func Check(cfg *config.Config, newProfile string, running map[string]bool, stateDir string) CheckResult {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	return CheckWithRAM(cfg, newProfile, running, stateDir, GetSystemRAM())
}

// CheckDefault is a convenience wrapper that uses the default state directory
// and queries GetSystemRAM. Callers that cannot inject the stateDir may use
// this form.
func CheckDefault(cfg *config.Config, newProfile string, running map[string]bool) CheckResult {
	return Check(cfg, newProfile, running, "")
}

// CheckWithRAM is the testable variant of Check. stateDir and systemRAMGB are
// injected so that tests can operate without touching the real filesystem or
// querying sysctl.
func CheckWithRAM(cfg *config.Config, newProfile string, running map[string]bool, stateDir string, systemRAMGB int) CheckResult {
	budget := cfg.MemoryBudget
	if budget == 0 {
		budget = config.CalculateBudget(systemRAMGB)
	}

	newMemory := profileMemory(cfg, newProfile)

	// Sum the memory of all currently running VMs.
	used := 0
	for name := range running {
		used += profileMemory(cfg, name)
	}

	exceeded := (used + newMemory) > budget

	var candidates []Candidate
	if exceeded {
		candidates = buildCandidates(cfg, running, stateDir)
	}

	return CheckResult{
		Exceeded:   exceeded,
		Used:       used,
		Budget:     budget,
		NewProfile: newProfile,
		NewMemory:  newMemory,
		Candidates: candidates,
	}
}

// FormatWarning returns a multi-line human-readable warning suitable for
// interactive terminal output when the budget has been exceeded.
//
// Example output:
//
//	⚠ Memory budget exceeded: 18GB would be used of 16GB budget (currently 14GB)
//	  Running environments:
//	    personal   4GB  (idle 3h)
//	    innolumi   6GB  (idle 45m)
//	    default    4GB  (active)
func (r CheckResult) FormatWarning() string {
	var sb strings.Builder

	totalIfStarted := r.Used + r.NewMemory
	fmt.Fprintf(&sb, "⚠ Memory budget exceeded: %dGB would be used of %dGB budget (currently %dGB)\n",
		totalIfStarted, r.Budget, r.Used)
	sb.WriteString("  Running environments:\n")

	for _, c := range r.Candidates {
		idleStr := formatIdleLabel(c.Idle)
		fmt.Fprintf(&sb, "    %-12s %dGB  (%s)\n", c.Name, c.Memory, idleStr)
	}

	return sb.String()
}

// FormatSuggestion returns a single-line prompt asking the user to stop the
// longest-idle VM. It is intended for the interactive "proceed anyway?" flow.
//
// Example output:
//
//	Stop personal (idle 3h, frees 4GB) to free memory? [y/N]
func (r CheckResult) FormatSuggestion() string {
	if len(r.Candidates) == 0 {
		return ""
	}
	top := r.Candidates[0]
	idleStr := formatIdleLabel(top.Idle)
	return fmt.Sprintf("Stop %s (%s, frees %dGB) to free memory? [y/N]\n  cloister stop %s",
		top.Name, idleStr, top.Memory, top.Name)
}

// FormatNonInteractive returns an error message with a remediation suggestion
// for use in scripts or CI pipelines where interactive prompting is not
// possible.
//
// Example output:
//
//	Error: memory budget exceeded (18GB/16GB)
//	  Suggestion: cloister stop personal  # idle 3h, frees 4GB
func (r CheckResult) FormatNonInteractive() string {
	totalIfStarted := r.Used + r.NewMemory
	msg := fmt.Sprintf("Error: memory budget exceeded (%dGB/%dGB)", totalIfStarted, r.Budget)

	if len(r.Candidates) > 0 {
		top := r.Candidates[0]
		idleStr := formatIdleLabel(top.Idle)
		msg += fmt.Sprintf("\n  Suggestion: cloister stop %s  # %s, frees %dGB",
			top.Name, idleStr, top.Memory)
	}

	return msg
}

// GetSystemRAM returns the total installed RAM in gigabytes by querying the
// macOS sysctl key hw.memsize. The value is returned in bytes by sysctl and
// converted to whole gigabytes. If the sysctl call fails or cannot be parsed,
// 16 is returned as a safe fallback.
func GetSystemRAM() int {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 16
	}

	trimmed := strings.TrimSpace(string(out))
	bytes, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || bytes <= 0 {
		return 16
	}

	// Convert bytes to gigabytes using integer division (1 GB = 1,073,741,824 bytes).
	gb := int(bytes / (1024 * 1024 * 1024))
	if gb <= 0 {
		return 16
	}
	return gb
}

// profileMemory returns the memory allocation in GB for the named profile.
// When the profile is not found or its memory is unset, the default is used.
func profileMemory(cfg *config.Config, name string) int {
	if p, ok := cfg.Profiles[name]; ok && p.Memory > 0 {
		return p.Memory
	}
	return config.DefaultMemory
}

// buildCandidates constructs the list of running VMs eligible for eviction,
// reading idle timestamps from stateDir and sorting by descending idle time.
func buildCandidates(cfg *config.Config, running map[string]bool, stateDir string) []Candidate {
	candidates := make([]Candidate, 0, len(running))

	for name := range running {
		idle := readIdleDuration(stateDir, name)
		candidates = append(candidates, Candidate{
			Name:   name,
			Memory: profileMemory(cfg, name),
			Idle:   idle,
		})
	}

	// Sort descending by idle duration so the longest-idle VM is first and
	// is the primary eviction suggestion.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Idle > candidates[j].Idle
	})

	return candidates
}

// readIdleDuration reads the Unix timestamp from stateDir/<profile>.last_entry
// and returns how long ago that timestamp was. If the file is missing or
// unreadable, zero is returned so the profile sorts last among candidates.
func readIdleDuration(stateDir, profile string) time.Duration {
	path := filepath.Join(stateDir, profile+".last_entry")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}

	return time.Since(time.Unix(ts, 0))
}

// defaultStateDir returns the path to the state directory used for idle
// timestamp files (~/.cloister/state). Errors are swallowed because callers
// fall back gracefully when the path is unusable.
func defaultStateDir() string {
	dir, err := config.ConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "state")
}

// formatIdleLabel converts an idle duration into a human-readable label.
// Zero or near-zero durations are reported as "active".
func formatIdleLabel(d time.Duration) string {
	if d < time.Minute {
		return "active"
	}

	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60

	if hours > 0 && minutes > 0 {
		return fmt.Sprintf("idle %dh%dm", hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("idle %dh", hours)
	}
	return fmt.Sprintf("idle %dm", minutes)
}
