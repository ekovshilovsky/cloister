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

func TestGuestGHShimRecordsRealBinaryAndProxiesInsideWorkspace(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(home, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	realGH := filepath.Join(fakeBin, "gh")
	if err := os.WriteFile(realGH, []byte("#!/bin/sh\nprintf 'guest-gh:%s\\n' \"$*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	install := exec.Command("bash", "-c", guestInstallScript)
	install.Env = []string{"HOME=" + home, "PATH=" + fakeBin + ":/usr/bin:/bin"}
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("installing shim: %v: %s", err, output)
	}
	recorded, err := os.ReadFile(filepath.Join(home, ".cloister", "bin", "gh.real-path"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(recorded)); got != realGH {
		t.Fatalf("recorded real gh = %q, want %q", got, realGH)
	}
	shim := filepath.Join(home, ".local", "bin", "gh")
	target, err := os.Readlink(shim)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".cloister", "lib", "vcs-shim"); target != want {
		t.Fatalf("gh shim target = %q, want %q", target, want)
	}

	capture := filepath.Join(home, "curl-args")
	fakeCurl := `#!/bin/sh
headers=""
while [ "$#" -gt 0 ]; do
    printf '%s\n' "$1" >> "$CAPTURE"
    if [ "$1" = "-D" ]; then
        shift
        headers="$1"
        printf '%s\n' "$1" >> "$CAPTURE"
    fi
    shift
done
printf 'x-cloister-exit-code: 0\r\n' > "$headers"
printf 'host-gh\n'
`
	if err := os.WriteFile(filepath.Join(fakeBin, "curl"), []byte(fakeCurl), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "CLOISTER_VCS_URL='http://127.0.0.1:49231/v1/exec'\nCLOISTER_VCS_TOKEN='token'\n"
	if err := os.WriteFile(filepath.Join(home, ".cloister", "vcs-broker.env"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	insideDir := filepath.Join(home, "workspaces", "project-123")
	if err := os.MkdirAll(insideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := exec.Command("bash", "-c", "gh pr status")
	inside.Dir = insideDir
	inside.Env = []string{"HOME=" + home, "PATH=" + filepath.Join(home, ".local", "bin") + ":" + fakeBin + ":/usr/bin:/bin", "CAPTURE=" + capture}
	output, err := inside.CombinedOutput()
	if err != nil || string(output) != "host-gh\n" {
		t.Fatalf("brokered gh error=%v output=%q", err, output)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tool=gh", "arg=pr", "arg=status", "http://127.0.0.1:49231/v1/exec"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("broker curl arguments missing %q:\n%s", want, args)
		}
	}
}

func TestGuestGHShimFallsBackWhenBaseIsInstalledAfterShim(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	toolBin := filepath.Join(home, "tools")
	if err := os.MkdirAll(toolBin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "basename", "cat", "chmod", "ln", "mkdir"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(toolBin, name)); err != nil {
			t.Fatal(err)
		}
	}

	// Replace the fixed guest system directory with a temporary equivalent so
	// this Linux provisioning order can be exercised on any test host.
	systemBin := filepath.Join(home, "system-bin")
	if err := os.MkdirAll(systemBin, 0o700); err != nil {
		t.Fatal(err)
	}
	testInstallScript := strings.ReplaceAll(guestInstallScript, `"/usr/bin/$tool"`, `"`+systemBin+`/$tool"`)
	if !strings.Contains(testInstallScript, systemBin) {
		t.Fatal("test system directory was not substituted into the guest shim")
	}
	install := exec.Command("bash", "-c", testInstallScript)
	install.Env = []string{"HOME=" + home, "PATH=" + toolBin}
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("installing shim before gh: %v: %s", err, output)
	}

	realFile := filepath.Join(home, ".cloister", "bin", "gh.real-path")
	recorded, err := os.ReadFile(realFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(recorded)) != "" {
		t.Fatalf("pre-gh real path = %q, want empty legacy record", recorded)
	}
	realGH := filepath.Join(systemBin, "gh")
	if err := os.WriteFile(realGH, []byte("#!/bin/sh\nprintf 'base-gh:%s\\n' \"$*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	installedShim, err := os.ReadFile(filepath.Join(home, ".cloister", "lib", "vcs-shim"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installedShim), systemBin) {
		t.Fatalf("installed shim does not contain test system directory:\n%s", installedShim)
	}

	shim := filepath.Join(home, ".local", "bin", "gh")
	outside := exec.Command(shim, "--version")
	outside.Dir = home
	outside.Env = []string{"HOME=" + home, "PATH=" + toolBin}
	output, err := outside.CombinedOutput()
	if err != nil || string(output) != "base-gh:--version\n" {
		t.Fatalf("reverse-order fallback error=%v output=%q", err, output)
	}
	recorded, err = os.ReadFile(realFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(recorded)) != "" {
		t.Fatalf("fallback rewrote legacy real path to %q", recorded)
	}
}

func TestGuestGHShimDoesNotRewriteExistingRealPath(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldBin := filepath.Join(home, "old-bin")
	newBin := filepath.Join(home, "new-bin")
	if err := os.MkdirAll(filepath.Join(home, ".cloister", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(oldBin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newBin, 0o700); err != nil {
		t.Fatal(err)
	}
	oldGH := filepath.Join(oldBin, "gh")
	newGH := filepath.Join(newBin, "gh")
	if err := os.WriteFile(oldGH, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newGH, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(home, ".cloister", "bin", "gh.real-path")
	if err := os.WriteFile(realFile, []byte(oldGH+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	install := exec.Command("bash", "-c", guestInstallScript)
	install.Env = []string{"HOME=" + home, "PATH=" + newBin + ":/usr/bin:/bin"}
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("reinstalling shim: %v: %s", err, output)
	}
	recorded, err := os.ReadFile(realFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(recorded)); got != oldGH {
		t.Fatalf("existing real path rewritten to %q, want %q", got, oldGH)
	}
}

func TestRemoveGuestConfigUsesMappedProfile(t *testing.T) {
	backend := &vm.MockBackend{}
	RemoveGuestConfig(backend, "work")
	if len(backend.SSHScriptCalls) != 1 || backend.SSHScriptCalls[0].Profile != "work" || !strings.Contains(backend.SSHScriptCalls[0].Script, "vcs-broker.env") {
		t.Fatalf("SSH script calls = %#v", backend.SSHScriptCalls)
	}
}
