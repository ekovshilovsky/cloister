package vcsbroker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cloister.io/internal/vm"
)

func TestDeployGuestInstallsAuthenticatedGitAndGHShims(t *testing.T) {
	backend := &vm.MockBackend{}
	if err := DeployGuest(backend, "work", 49231, "012345abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(backend.SSHScriptCalls) != 1 || backend.SSHScriptCalls[0].Profile != "work" {
		t.Fatalf("SSH script calls = %#v", backend.SSHScriptCalls)
	}
	script := backend.SSHScriptCalls[0].Script
	for _, required := range []string{
		"http://127.0.0.1:49231/v1/exec",
		"CLOISTER_VCS_TOKEN='012345abcdef'",
		`ln -sfn "$HOME/.cloister/lib/vcs-shim" "$HOME/.local/bin/git"`,
		`ln -sfn "$HOME/.cloister/lib/vcs-shim" "$HOME/.local/bin/gh"`,
		"outside_mapped=true",
		`env=GH_REPO=$GH_REPO`,
		"x-cloister-exit-code:",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("guest deployment script missing %q", required)
		}
	}
}

func TestDeployGuestRejectsUnsafeConfigurationAndSurfacesBackendFailure(t *testing.T) {
	for _, tc := range []struct {
		port  int
		token string
	}{
		{port: 0, token: "token"},
		{port: 65536, token: "token"},
		{port: 49231, token: ""},
		{port: 49231, token: "bad'token"},
		{port: 49231, token: "bad\ntoken"},
	} {
		if err := DeployGuest(&vm.MockBackend{}, "work", tc.port, tc.token); err == nil {
			t.Fatalf("DeployGuest(%d, %q) succeeded", tc.port, tc.token)
		}
	}
	backend := &vm.MockBackend{SSHScriptErr: errors.New("guest unavailable")}
	if err := DeployGuest(backend, "work", 49231, "token"); err == nil || !strings.Contains(err.Error(), "guest unavailable") {
		t.Fatalf("DeployGuest() error = %v", err)
	}
}

func TestGuestShimFallsThroughOutsideWorkspaceAndFailsClosedInside(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(home, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	realGit := filepath.Join(fakeBin, "git")
	if err := os.WriteFile(realGit, []byte("#!/bin/sh\nprintf 'guest-git:%s\\n' \"$*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	install := exec.Command("bash", "-c", guestInstallScript)
	install.Env = []string{"HOME=" + home, "PATH=" + fakeBin + ":/usr/bin:/bin"}
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("installing shim: %v: %s", err, output)
	}

	shim := filepath.Join(home, ".local", "bin", "git")
	outside := exec.Command(shim, "status")
	outside.Dir = home
	outside.Env = []string{"HOME=" + home, "PATH=" + fakeBin + ":/usr/bin:/bin"}
	output, err := outside.CombinedOutput()
	if err != nil || string(output) != "guest-git:status\n" {
		t.Fatalf("outside fallback error=%v output=%q", err, output)
	}

	insideDir := filepath.Join(home, "workspaces", "project-123")
	if err := os.MkdirAll(insideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := exec.Command(shim, "status")
	inside.Dir = insideDir
	inside.Env = []string{"HOME=" + home, "PATH=" + fakeBin + ":/usr/bin:/bin"}
	output, err = inside.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 125 || !strings.Contains(string(output), "VCS broker is unavailable") {
		t.Fatalf("inside fail-closed error=%v output=%q", err, output)
	}
}

func TestRemoveGuestConfigUsesMappedProfile(t *testing.T) {
	backend := &vm.MockBackend{}
	RemoveGuestConfig(backend, "work")
	if len(backend.SSHScriptCalls) != 1 || backend.SSHScriptCalls[0].Profile != "work" || !strings.Contains(backend.SSHScriptCalls[0].Script, "vcs-broker.env") {
		t.Fatalf("SSH script calls = %#v", backend.SSHScriptCalls)
	}
}
