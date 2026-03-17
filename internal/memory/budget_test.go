package memory_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/memory"
)

// buildConfig constructs a minimal Config with the given profiles and an
// optional explicit memory budget (0 means auto-calculate from systemRAM).
func buildConfig(t *testing.T, budgetGB int, profiles map[string]int) (*config.Config, string) {
	t.Helper()

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}

	cfg := &config.Config{
		MemoryBudget: budgetGB,
		Profiles:     make(map[string]*config.Profile),
	}
	for name, memGB := range profiles {
		cfg.Profiles[name] = &config.Profile{Memory: memGB}
	}
	return cfg, stateDir
}

// writeLastEntry writes a Unix timestamp to stateDir/<profile>.last_entry,
// simulating a prior cloister enter invocation that occurred idleSince ago.
func writeLastEntry(t *testing.T, stateDir, profile string, idleSince time.Duration) {
	t.Helper()
	ts := time.Now().Add(-idleSince).Unix()
	path := filepath.Join(stateDir, profile+".last_entry")
	if err := os.WriteFile(path, []byte(strconv.FormatInt(ts, 10)), 0o600); err != nil {
		t.Fatalf("writing last_entry for %s: %v", profile, err)
	}
}

// TestCheckUnderBudget verifies that Check reports Exceeded=false when the
// running VMs plus the new profile fit within the configured budget.
func TestCheckUnderBudget(t *testing.T) {
	cfg, stateDir := buildConfig(t, 8, map[string]int{
		"existing": 4,
		"new":      4,
	})

	running := map[string]bool{"existing": true}
	result := memory.Check(cfg, "new", running, stateDir)

	if result.Exceeded {
		t.Errorf("expected Exceeded=false for 4GB running + 4GB new within 8GB budget, got Exceeded=true")
	}
	if result.Used != 4 {
		t.Errorf("Used: got %d, want 4", result.Used)
	}
	if result.Budget != 8 {
		t.Errorf("Budget: got %d, want 8", result.Budget)
	}
	if result.NewProfile != "new" {
		t.Errorf("NewProfile: got %q, want %q", result.NewProfile, "new")
	}
	if result.NewMemory != 4 {
		t.Errorf("NewMemory: got %d, want 4", result.NewMemory)
	}
	if len(result.Candidates) != 0 {
		t.Errorf("expected no candidates when under budget, got %d", len(result.Candidates))
	}
}

// TestCheckOverBudget verifies that Check reports Exceeded=true with correctly
// populated candidates sorted by descending idle duration when the running VMs
// plus the new profile exceed the configured budget.
func TestCheckOverBudget(t *testing.T) {
	cfg, stateDir := buildConfig(t, 8, map[string]int{
		"personal": 4,
		"innolumi": 4,
		"newenv":   4,
	})

	// personal has been idle the longest; innolumi was entered more recently.
	writeLastEntry(t, stateDir, "personal", 3*time.Hour)
	writeLastEntry(t, stateDir, "innolumi", 45*time.Minute)

	running := map[string]bool{"personal": true, "innolumi": true}
	result := memory.Check(cfg, "newenv", running, stateDir)

	if !result.Exceeded {
		t.Errorf("expected Exceeded=true for 8GB running + 4GB new exceeding 8GB budget")
	}
	if result.Used != 8 {
		t.Errorf("Used: got %d, want 8", result.Used)
	}
	if result.Budget != 8 {
		t.Errorf("Budget: got %d, want 8", result.Budget)
	}
	if result.NewMemory != 4 {
		t.Errorf("NewMemory: got %d, want 4", result.NewMemory)
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(result.Candidates))
	}

	// Candidates must be ordered by descending idle time: personal first.
	if result.Candidates[0].Name != "personal" {
		t.Errorf("Candidates[0].Name: got %q, want %q", result.Candidates[0].Name, "personal")
	}
	if result.Candidates[1].Name != "innolumi" {
		t.Errorf("Candidates[1].Name: got %q, want %q", result.Candidates[1].Name, "innolumi")
	}

	// Verify idle durations are populated and correctly ordered.
	if result.Candidates[0].Idle < result.Candidates[1].Idle {
		t.Errorf("Candidates[0] idle (%v) should be >= Candidates[1] idle (%v)",
			result.Candidates[0].Idle, result.Candidates[1].Idle)
	}
}

// TestCheckOverBudgetNoLastEntry verifies that profiles without a
// .last_entry file are included as candidates with zero idle duration,
// placed after any profiles that have recorded idle times.
func TestCheckOverBudgetNoLastEntry(t *testing.T) {
	cfg, stateDir := buildConfig(t, 4, map[string]int{
		"alpha": 4,
		"beta":  4,
	})

	// alpha has a last_entry; beta does not.
	writeLastEntry(t, stateDir, "alpha", 2*time.Hour)

	running := map[string]bool{"alpha": true}
	result := memory.Check(cfg, "beta", running, stateDir)

	if !result.Exceeded {
		t.Errorf("expected Exceeded=true")
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(result.Candidates))
	}
	if result.Candidates[0].Name != "alpha" {
		t.Errorf("Candidates[0].Name: got %q, want %q", result.Candidates[0].Name, "alpha")
	}
}

// TestCheckAutoBudget verifies that when MemoryBudget is 0 the budget is
// derived from CalculateBudget rather than a fixed value.
func TestCheckAutoBudget(t *testing.T) {
	// Use a fixed system RAM so the test is deterministic.
	const fakeRAM = 20 // CalculateBudget(20) = 10
	expectedBudget := config.CalculateBudget(fakeRAM)

	cfg, stateDir := buildConfig(t, 0, map[string]int{
		"env": 4,
	})

	running := map[string]bool{}
	result := memory.CheckWithRAM(cfg, "env", running, stateDir, fakeRAM)

	if result.Budget != expectedBudget {
		t.Errorf("Budget: got %d, want %d (CalculateBudget(%d))", result.Budget, expectedBudget, fakeRAM)
	}
}

// TestCheckExplicitBudgetOverridesRAM verifies that a non-zero MemoryBudget in
// the config is used as-is without consulting GetSystemRAM.
func TestCheckExplicitBudgetOverridesRAM(t *testing.T) {
	cfg, stateDir := buildConfig(t, 16, map[string]int{
		"env": 4,
	})

	// Provide an obviously-wrong system RAM to confirm it is ignored.
	running := map[string]bool{}
	result := memory.CheckWithRAM(cfg, "env", running, stateDir, 9999)

	if result.Budget != 16 {
		t.Errorf("Budget: got %d, want 16 (explicit config budget should be used)", result.Budget)
	}
}

// TestFormatWarning verifies that FormatWarning produces output containing the
// key data points: memory totals, budget, and candidate idle times.
func TestFormatWarning(t *testing.T) {
	result := memory.CheckResult{
		Exceeded:   true,
		Used:       14,
		Budget:     12,
		NewProfile: "newenv",
		NewMemory:  4,
		Candidates: []memory.Candidate{
			{Name: "personal", Memory: 4, Idle: 3*time.Hour + 15*time.Minute},
			{Name: "innolumi", Memory: 6, Idle: 45 * time.Minute},
			{Name: "default", Memory: 4, Idle: 0},
		},
	}

	warning := result.FormatWarning()

	checks := []string{
		"14GB",
		"12GB",
		"personal",
		"innolumi",
		"default",
	}
	for _, want := range checks {
		if !strings.Contains(warning, want) {
			t.Errorf("FormatWarning output missing %q\nGot:\n%s", want, warning)
		}
	}
}

// TestFormatSuggestion verifies that FormatSuggestion names the longest-idle
// candidate in the suggested stop command.
func TestFormatSuggestion(t *testing.T) {
	result := memory.CheckResult{
		Exceeded:   true,
		Used:       8,
		Budget:     8,
		NewProfile: "new",
		NewMemory:  4,
		Candidates: []memory.Candidate{
			{Name: "personal", Memory: 4, Idle: 3 * time.Hour},
			{Name: "innolumi", Memory: 6, Idle: 30 * time.Minute},
		},
	}

	suggestion := result.FormatSuggestion()

	if !strings.Contains(suggestion, "personal") {
		t.Errorf("FormatSuggestion should suggest stopping the longest-idle VM 'personal'\nGot: %s", suggestion)
	}
	if !strings.Contains(suggestion, "cloister stop") {
		t.Errorf("FormatSuggestion should include 'cloister stop'\nGot: %s", suggestion)
	}
}

// TestFormatNonInteractive verifies that the non-interactive error format
// includes the budget ratio and the suggested stop command.
func TestFormatNonInteractive(t *testing.T) {
	result := memory.CheckResult{
		Exceeded:   true,
		Used:       8,
		Budget:     8,
		NewProfile: "new",
		NewMemory:  4,
		Candidates: []memory.Candidate{
			{Name: "personal", Memory: 4, Idle: 3 * time.Hour},
		},
	}

	msg := result.FormatNonInteractive()

	wantSubstrings := []string{
		"Error:",
		"personal",
		"cloister stop",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(msg, want) {
			t.Errorf("FormatNonInteractive output missing %q\nGot: %s", want, msg)
		}
	}
}

// TestFormatNonInteractiveNoCandidates verifies that FormatNonInteractive
// degrades gracefully when no candidates are available.
func TestFormatNonInteractiveNoCandidates(t *testing.T) {
	result := memory.CheckResult{
		Exceeded:   true,
		Used:       8,
		Budget:     8,
		NewProfile: "new",
		NewMemory:  4,
		Candidates: nil,
	}

	msg := result.FormatNonInteractive()
	if !strings.Contains(msg, "Error:") {
		t.Errorf("FormatNonInteractive should contain 'Error:'\nGot: %s", msg)
	}
}

// TestGetSystemRAM verifies that GetSystemRAM returns a positive value on macOS.
// On other platforms the test is skipped because sysctl is not available.
func TestGetSystemRAM(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("GetSystemRAM integration test only runs on macOS")
	}

	ram := memory.GetSystemRAM()
	if ram <= 0 {
		t.Errorf("GetSystemRAM() = %d, want > 0", ram)
	}

	// Sanity check: modern Macs have at least 8GB and the result should be
	// in whole gigabytes.
	if ram < 4 {
		t.Errorf("GetSystemRAM() = %dGB, expected at least 4GB on a Mac", ram)
	}

	fmt.Printf("GetSystemRAM() = %dGB\n", ram)
}
