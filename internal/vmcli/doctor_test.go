package vmcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckConfigFilePresent(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte(`{"profile":"test"}`), 0o600)

	result := checkConfigFileAt(configPath)
	if result.Status != "pass" {
		t.Errorf("expected pass for existing config, got %q: %s", result.Status, result.Detail)
	}
}

func TestCheckConfigFileMissing(t *testing.T) {
	result := checkConfigFileAt("/nonexistent/config.json")
	if result.Status != "fail" {
		t.Errorf("expected fail for missing config, got %q", result.Status)
	}
}

func TestCheckConfigFileMalformed(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte(`{invalid`), 0o600)

	result := checkConfigFileAt(configPath)
	if result.Status != "fail" {
		t.Errorf("expected fail for malformed config, got %q", result.Status)
	}
}

func TestCheckResultFormat(t *testing.T) {
	r := CheckResult{Name: "test-check", Status: "pass", Detail: "all good"}
	s := r.String()
	if s == "" {
		t.Error("String() should produce output")
	}
}

func TestCheckResultStringPass(t *testing.T) {
	r := CheckResult{Name: "config", Status: "pass", Detail: "found"}
	s := r.String()
	if len(s) == 0 {
		t.Fatal("expected non-empty string")
	}
	// Should contain the pass indicator
	if !strings.Contains(s, "config") {
		t.Errorf("expected check name in output, got %q", s)
	}
}

func TestCheckResultStringFail(t *testing.T) {
	r := CheckResult{Name: "config", Status: "fail", Detail: "missing"}
	s := r.String()
	if len(s) == 0 {
		t.Fatal("expected non-empty string")
	}
}

func TestCheckResultStringWarn(t *testing.T) {
	r := CheckResult{Name: "swap", Status: "warn", Detail: "not configured"}
	s := r.String()
	if len(s) == 0 {
		t.Fatal("expected non-empty string")
	}
}

func TestDoctorCheckTimeout(t *testing.T) {
	// Simulate a check that hangs by probing a non-routable IP address
	result := runCheckWithTimeout("slow-check", func() CheckResult {
		ProbeTCP("192.0.2.1", 1, 100*time.Millisecond) // non-routable, will timeout
		return CheckResult{Name: "slow-check", Status: "pass"}
	}, 200*time.Millisecond)
	// Should complete within timeout, not hang
	if result.Name != "slow-check" {
		t.Error("check should return a result even on timeout")
	}
}

func TestDoctorCheckTimeoutExpired(t *testing.T) {
	// Verify that a genuinely slow check is terminated by the timeout
	result := runCheckWithTimeout("hanging-check", func() CheckResult {
		time.Sleep(2 * time.Second)
		return CheckResult{Name: "hanging-check", Status: "pass"}
	}, 100*time.Millisecond)
	if result.Status != "fail" {
		t.Errorf("expected fail for timed-out check, got %q", result.Status)
	}
}

func TestCheckClaudeMode(t *testing.T) {
	// Save and restore the env var
	orig := os.Getenv("ANTHROPIC_BASE_URL")
	defer os.Setenv("ANTHROPIC_BASE_URL", orig)

	os.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:11434")
	result := checkClaudeMode()
	if result.Status != "pass" {
		t.Errorf("expected pass, got %q", result.Status)
	}
	if !strings.Contains(result.Detail, "local") {
		t.Errorf("expected 'local' in detail, got %q", result.Detail)
	}

	os.Unsetenv("ANTHROPIC_BASE_URL")
	result = checkClaudeMode()
	if result.Status != "pass" {
		t.Errorf("expected pass for cloud mode, got %q", result.Status)
	}
	if !strings.Contains(result.Detail, "cloud") {
		t.Errorf("expected 'cloud' in detail, got %q", result.Detail)
	}
}

func TestFormatDoctorSummary(t *testing.T) {
	results := []CheckResult{
		{Name: "a", Status: "pass"},
		{Name: "b", Status: "pass"},
		{Name: "c", Status: "warn"},
		{Name: "d", Status: "fail"},
	}
	summary := FormatDoctorSummary(results)
	if summary != "2 passed, 1 warnings, 1 failed" {
		t.Errorf("unexpected summary: %q", summary)
	}
}

func TestCheckOpForwardTokenFile(t *testing.T) {
	// Non-existent path should warn
	result := checkOpForwardTokenFileAt("/nonexistent/refresh.token")
	if result.Status != "warn" {
		t.Errorf("expected warn for missing token, got %q", result.Status)
	}

	// Existing file should pass
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "refresh.token")
	os.WriteFile(tokenPath, []byte("token-data"), 0o600)
	result = checkOpForwardTokenFileAt(tokenPath)
	if result.Status != "pass" {
		t.Errorf("expected pass for existing token, got %q: %s", result.Status, result.Detail)
	}
}
