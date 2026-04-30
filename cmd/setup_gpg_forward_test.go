package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsurePinentryProgramIdempotentWhenAlreadySet(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "gpg-agent.conf")
	initial := "default-cache-ttl 28800\npinentry-program /opt/homebrew/bin/pinentry-mac\n"
	if err := os.WriteFile(confPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	changed, err := ensurePinentryProgram(confPath, "/opt/homebrew/bin/pinentry-mac", false)
	if err != nil {
		t.Fatalf("ensurePinentryProgram: %v", err)
	}
	if changed {
		t.Errorf("expected no change when pinentry-program already correct")
	}

	out, _ := os.ReadFile(confPath)
	if strings.Count(string(out), "pinentry-program") != 1 {
		t.Errorf("expected exactly one pinentry-program line, got:\n%s", out)
	}
}

func TestEnsurePinentryProgramAppendsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "gpg-agent.conf")
	if err := os.WriteFile(confPath, []byte("default-cache-ttl 28800\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	changed, err := ensurePinentryProgram(confPath, "/opt/homebrew/bin/pinentry-mac", false)
	if err != nil {
		t.Fatalf("ensurePinentryProgram: %v", err)
	}
	if !changed {
		t.Errorf("expected change when pinentry-program absent")
	}

	out, _ := os.ReadFile(confPath)
	if !strings.Contains(string(out), "pinentry-program /opt/homebrew/bin/pinentry-mac") {
		t.Errorf("expected appended pinentry-program line; got:\n%s", out)
	}
}

func TestEnsurePinentryProgramRefusesSilentOverwrite(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "gpg-agent.conf")
	initial := "pinentry-program /usr/local/bin/pinentry-curses\n"
	if err := os.WriteFile(confPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	// confirmOverwrite=false → must not modify file.
	changed, err := ensurePinentryProgram(confPath, "/opt/homebrew/bin/pinentry-mac", false)
	if err == nil {
		t.Fatalf("expected error when existing pinentry-program differs and confirmOverwrite=false")
	}
	if changed {
		t.Errorf("file must not be modified when overwrite is refused")
	}

	out, _ := os.ReadFile(confPath)
	if !strings.Contains(string(out), "pinentry-curses") {
		t.Errorf("original pinentry-program line was clobbered; got:\n%s", out)
	}
}
