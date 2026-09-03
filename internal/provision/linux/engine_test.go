package linux

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"time"

	"cloister.io/internal/config"
	"cloister.io/internal/runlog"
	"cloister.io/internal/vm"
)

// embeddedScripts enumerates all scripts that must be present in the embedded
// filesystem. This list is the authoritative record of what provisioning
// delivers and should be kept in sync with the scripts/ directory.
var embeddedScripts = []string{
	"scripts/base.sh",
	"scripts/stack-web.sh",
	"scripts/stack-cloud.sh",
	"scripts/stack-python.sh",
	"scripts/stack-dotnet.sh",
	"scripts/stack-go.sh",
	"scripts/stack-rust.sh",
	"scripts/stack-data.sh",
	"scripts/stack-ollama.sh",
	"scripts/stack-art.sh",
	"scripts/stack-office.sh",
	"scripts/stack-agentgrid.sh",
	"scripts/read-only-mounts.sh",
}

// embeddedTemplates enumerates all templates that must be present in the
// embedded filesystem.
var embeddedTemplates = []string{
	"templates/bashrc.tmpl",
	"templates/gitconfig.tmpl",
}

// TestEmbeddedScriptsExist verifies that every required provisioning script
// was embedded at compile time and can be read without error.
func TestEmbeddedScriptsExist(t *testing.T) {
	t.Parallel()
	for _, path := range embeddedScripts {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			data, err := Scripts.ReadFile(path)
			if err != nil {
				t.Fatalf("Scripts.ReadFile(%q): %v", path, err)
			}
			if len(data) == 0 {
				t.Fatalf("Scripts.ReadFile(%q): file is empty", path)
			}
		})
	}
}

func TestAgentGridStackDownloadsOfficialArchitectureAsset(t *testing.T) {
	t.Parallel()

	data, err := Scripts.ReadFile("scripts/stack-agentgrid.sh")
	if err != nil {
		t.Fatalf("Scripts.ReadFile(stack-agentgrid.sh): %v", err)
	}
	script := string(data)

	for _, want := range []string{
		"https://api.github.com/repos/agent-grid/agent-grid-releases/releases/latest",
		`AgentGrid-${VERSION}-${DEB_ARCH}.deb`,
		`releases/download/${TAG}/${ASSET_NAME}`,
		`curl --no-progress-meter -fL --retry 3 --retry-delay 2`,
		"systemctl --user is-active --quiet agent-grid-daemon.service",
		"Agent Grid daemon failed to start",
		"AGENT_GRID_IDLE_SHUTDOWN_MS=0",
		// systemd, not the desktop app, launches the daemon in the VM, so the
		// unit must set the daemon flag itself. Without it daemonClientEnabled()
		// is false in every master agent and worker spawning is blocked.
		"Environment=AGENT_GRID_DAEMON=1",
		// Agent CLIs (claude, cursor-agent) live in ~/.local/bin; without this
		// PATH the daemon's `which` probes report every agent as "not found".
		"Environment=PATH=%h/.local/bin:",
		// The Claude SDK worker backend needs its bundled native binary
		// resolved explicitly; under ELECTRON_RUN_AS_NODE the daemon cannot
		// self-resolve it and spawning dies with ENOTDIR. The wrapper points
		// the env var at the arch/libc-correct unpacked binary.
		"AGENT_GRID_CLAUDE_CLI_PATH",
		"app.asar.unpacked/node_modules/@anthropic-ai/claude-agent-sdk-linux-",
		// Re-runs (cloister repair / addstack) must restart a live daemon when
		// the unit content changed, or new settings never reach the process.
		"systemctl --user restart agent-grid-daemon.service",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("agentgrid installer missing %q", want)
		}
	}

	// The stack installs every harness Agent Grid can drive, idempotently,
	// so a client connecting to the daemon finds them all. If a provider is
	// dropped here it silently stops being installed in new/repaired VMs.
	for _, binary := range []string{
		"claude", "codex", "cursor-agent", "opencode",
		"agy", "kimi", "grok", "devin", "pi",
	} {
		if !strings.Contains(script, "install_agent_cli") {
			t.Fatal("agentgrid installer missing the install_agent_cli helper")
		}
		if !strings.Contains(script, " "+binary+" ") {
			t.Errorf("agentgrid installer does not install the %q CLI", binary)
		}
	}
	// Idempotency: repair must skip an already-installed agent rather than
	// re-running its vendor script every time.
	if !strings.Contains(script, "command -v \"$binary\"") {
		t.Error("agentgrid installer must skip agents already on PATH (idempotent repair)")
	}

	// `enable --now` silently skips restarting an already-running daemon, so
	// unit changes written by a repair would never take effect. The script
	// must use the explicit enable + hash-compare + restart/start sequence.
	if strings.Contains(script, "enable --now") {
		t.Error("agentgrid installer must not use `enable --now`; unit changes require an explicit restart on re-runs")
	}
}

// TestEmbeddedTemplatesExist verifies that every required configuration
// template was embedded at compile time and can be read without error.
func TestEmbeddedTemplatesExist(t *testing.T) {
	t.Parallel()
	for _, path := range embeddedTemplates {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			data, err := Templates.ReadFile(path)
			if err != nil {
				t.Fatalf("Templates.ReadFile(%q): %v", path, err)
			}
			if len(data) == 0 {
				t.Fatalf("Templates.ReadFile(%q): file is empty", path)
			}
		})
	}
}

// TestBashrcTemplateParses verifies that bashrc.tmpl is syntactically valid
// Go template syntax and renders without error for both GPG-signing and
// non-GPG-signing profiles.
func TestBashrcTemplateParses(t *testing.T) {
	t.Parallel()

	raw, err := Templates.ReadFile("templates/bashrc.tmpl")
	if err != nil {
		t.Fatalf("reading bashrc.tmpl: %v", err)
	}

	tmpl, err := template.New("bashrc").Parse(string(raw))
	if err != nil {
		t.Fatalf("parsing bashrc.tmpl: %v", err)
	}

	cases := []struct {
		name string
		data bashrcTemplateData
	}{
		{
			name: "gpg_signing_enabled",
			data: bashrcTemplateData{
				Profile:    "dev",
				StartDir:   "~/code/myproject",
				GPGSigning: true,
			},
		},
		{
			name: "gpg_signing_disabled",
			data: bashrcTemplateData{
				Profile:    "work",
				StartDir:   "~/code",
				GPGSigning: false,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, tc.data); err != nil {
				t.Fatalf("executing bashrc.tmpl with data %+v: %v", tc.data, err)
			}
			out := buf.String()
			if !strings.Contains(out, tc.data.Profile) {
				t.Errorf("rendered bashrc missing profile name %q", tc.data.Profile)
			}
			if !strings.Contains(out, tc.data.StartDir) {
				t.Errorf("rendered bashrc missing start dir %q", tc.data.StartDir)
			}
			if strings.Contains(out, "GNUPGHOME") {
				t.Errorf("rendered bashrc must not contain GNUPGHOME (forwarded gpg-agent uses default path); got: %s", out)
			}
			if strings.Contains(out, ".gnupg-local") {
				t.Errorf("rendered bashrc must not reference .gnupg-local; got: %s", out)
			}
		})
	}
}

// TestBashrcTemplateGPGTTYCorrect ensures the bashrc template uses the correct
// GPG_TTY variable name (not the former typo GPP_TTY).
func TestBashrcTemplateGPGTTYCorrect(t *testing.T) {
	t.Parallel()

	raw, err := Templates.ReadFile("templates/bashrc.tmpl")
	if err != nil {
		t.Fatalf("reading bashrc.tmpl: %v", err)
	}

	content := string(raw)
	if strings.Contains(content, "GPP_TTY") {
		t.Error("bashrc.tmpl contains typo GPP_TTY; should be GPG_TTY")
	}
	if !strings.Contains(content, "GPG_TTY") {
		t.Error("bashrc.tmpl is missing GPG_TTY export")
	}
}

// TestGitconfigTemplateParses verifies that gitconfig.tmpl is syntactically
// valid Go template syntax and renders correctly for both GPG-signing and
// non-GPG-signing configurations.
func TestGitconfigTemplateParses(t *testing.T) {
	t.Parallel()

	raw, err := Templates.ReadFile("templates/gitconfig.tmpl")
	if err != nil {
		t.Fatalf("reading gitconfig.tmpl: %v", err)
	}

	tmpl, err := template.New("gitconfig").Parse(string(raw))
	if err != nil {
		t.Fatalf("parsing gitconfig.tmpl: %v", err)
	}

	type gitconfigData struct {
		GitName    string
		GitEmail   string
		GPGSigning bool
		GPGKeyID   string
	}

	cases := []struct {
		name string
		data gitconfigData
	}{
		{
			name: "with_gpg_signing",
			data: gitconfigData{
				GitName:    "Alice Example",
				GitEmail:   "alice@example.com",
				GPGSigning: true,
				GPGKeyID:   "DEADBEEF12345678",
			},
		},
		{
			name: "without_gpg_signing",
			data: gitconfigData{
				GitName:    "Bob Example",
				GitEmail:   "bob@example.com",
				GPGSigning: false,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, tc.data); err != nil {
				t.Fatalf("executing gitconfig.tmpl with data %+v: %v", tc.data, err)
			}
			out := buf.String()
			if !strings.Contains(out, tc.data.GitName) {
				t.Errorf("rendered gitconfig missing name %q", tc.data.GitName)
			}
			if !strings.Contains(out, tc.data.GitEmail) {
				t.Errorf("rendered gitconfig missing email %q", tc.data.GitEmail)
			}
			if tc.data.GPGSigning {
				if !strings.Contains(out, "gpgsign = true") {
					t.Errorf("rendered gitconfig missing gpgsign=true when GPGSigning=true")
				}
				if !strings.Contains(out, tc.data.GPGKeyID) {
					t.Errorf("rendered gitconfig missing GPG key ID %q", tc.data.GPGKeyID)
				}
			} else {
				if strings.Contains(out, "gpgsign") {
					t.Errorf("rendered gitconfig contains gpgsign when GPGSigning=false")
				}
			}
		})
	}
}

// TestBashrcDataDefaults verifies that bashrcData substitutes the fallback
// start directory when the profile configuration leaves StartDir empty.
func TestBashrcDataDefaults(t *testing.T) {
	t.Parallel()

	p := &config.Profile{
		GPGSigning: false,
		// StartDir intentionally left empty to exercise the default path.
	}
	data := bashrcData("myprofile", p)

	if data.Profile != "myprofile" {
		t.Errorf("Profile = %q; want %q", data.Profile, "myprofile")
	}
	if data.StartDir != "~/code" {
		t.Errorf("StartDir = %q; want %q", data.StartDir, "~/code")
	}
	if data.GPGSigning {
		t.Errorf("GPGSigning = true; want false")
	}
}

// TestBashrcDataCustomStartDir verifies that a non-empty StartDir in the
// profile is preserved verbatim in the template data.
func TestBashrcDataCustomStartDir(t *testing.T) {
	t.Parallel()

	p := &config.Profile{
		StartDir:   "~/code/myproject",
		GPGSigning: true,
	}
	data := bashrcData("work", p)

	if data.StartDir != "~/code/myproject" {
		t.Errorf("StartDir = %q; want %q", data.StartDir, "~/code/myproject")
	}
	if !data.GPGSigning {
		t.Errorf("GPGSigning = false; want true")
	}
}

// TestScriptShebangAndPipefail verifies that each embedded shell script begins
// with a bash shebang and, where applicable, enables strict error handling.
// Scripts that explicitly relax error handling (read-only-mounts.sh) are
// excluded from the pipefail check since they are intentionally lenient.
func TestScriptShebangAndPipefail(t *testing.T) {
	t.Parallel()

	// Scripts that deliberately omit set -euo pipefail because their logic
	// relies on best-effort commands (mount, etc.).
	pipefailExempt := map[string]bool{
		"scripts/read-only-mounts.sh": true,
	}

	for _, path := range embeddedScripts {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			data, err := Scripts.ReadFile(path)
			if err != nil {
				t.Fatalf("Scripts.ReadFile(%q): %v", path, err)
			}
			content := string(data)
			if !strings.HasPrefix(content, "#!/bin/bash") {
				t.Errorf("%s: missing #!/bin/bash shebang", path)
			}
			if !pipefailExempt[path] && !strings.Contains(content, "set -euo pipefail") {
				t.Errorf("%s: missing 'set -euo pipefail' strict mode", path)
			}
		})
	}
}

// TestCheckHostAvailable verifies that checkHost returns true when a TCP
// listener is accepting connections on the target port.
func TestCheckHostAvailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test listener: %v", err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	if !checkHost("127.0.0.1", port, 500*time.Millisecond) {
		t.Error("checkHost should return true for a listening port")
	}
}

// TestCheckHostUnavailable verifies that checkHost returns false when no
// process is listening on the target port within the given timeout.
func TestCheckHostUnavailable(t *testing.T) {
	// Port 59999 is above the registered range and is almost certainly unused
	// in a CI or developer environment.
	if checkHost("127.0.0.1", 59999, 100*time.Millisecond) {
		t.Error("checkHost should return false for a non-listening port")
	}
}

// TestDeployGPGKeysScriptHasNoPrivateKeyMaterial guards the redesign that
// switched commit signing from shipping the host's private GPG keys into the
// VM to forwarding the host gpg-agent socket over SSH. It asserts that the
// rendered provisioning script never references private-keys-v1.d, never
// base64-encodes any payload, and writes the load-bearing VM-side
// configuration (gpg.conf no-autostart, sshd drop-in StreamLocalBindUnlink).
// If a future change reintroduces private-key shipping, this test fails.
func TestDeployGPGKeysScriptHasNoPrivateKeyMaterial(t *testing.T) {
	script := buildDeployGPGKeysScriptForTest()
	if strings.Contains(script, "private-keys-v1.d") {
		t.Errorf("script must not reference private-keys-v1.d; got:\n%s", script)
	}
	if strings.Contains(script, "base64") {
		t.Errorf("script must not contain base64 encoding (private-key shipping); got:\n%s", script)
	}
	if !strings.Contains(script, "no-autostart") {
		t.Errorf("script must write no-autostart into gpg.conf; got:\n%s", script)
	}
	if !strings.Contains(script, "StreamLocalBindUnlink yes") {
		t.Errorf("script must write StreamLocalBindUnlink directive into sshd drop-in; got:\n%s", script)
	}
	if !strings.Contains(script, "/etc/ssh/sshd_config.d/cloister-gpg.conf") {
		t.Errorf("script must target sshd_config.d drop-in path; got:\n%s", script)
	}
}

// TestDeployGPGKeysScriptImportsBeforeGPGConf guards a load-bearing ordering
// invariant in the rendered provisioning script. The public-key import must
// run BEFORE gpg.conf is written, because gpg.conf carries the no-autostart
// directive — once it is in place, gpg refuses to spawn a transient agent
// for the import on a fresh VM where no agent is running, causing the import
// to fail. The script must therefore (a) remove any pre-existing gpg.conf
// before importing, (b) run the import without silencing stderr or exit
// status, and (c) write the final gpg.conf only after the import succeeds.
func TestDeployGPGKeysScriptImportsBeforeGPGConf(t *testing.T) {
	script := buildDeployGPGKeysScriptForTest()

	importIdx := strings.Index(script, "gpg --batch --import")
	if importIdx < 0 {
		t.Fatalf("script does not contain `gpg --batch --import`; got:\n%s", script)
	}

	gpgConfWriteIdx := strings.Index(script, "GPG_CONF_EOF")
	if gpgConfWriteIdx < 0 {
		t.Fatalf("script does not contain gpg.conf heredoc marker `GPG_CONF_EOF`; got:\n%s", script)
	}

	if importIdx > gpgConfWriteIdx {
		t.Errorf("import must run BEFORE gpg.conf is written; gpg --batch --import at index %d, GPG_CONF_EOF at index %d", importIdx, gpgConfWriteIdx)
	}

	// Re-runs require removing the prior gpg.conf so a stale no-autostart
	// directive does not block the import.
	if !strings.Contains(script, "rm -f \"$HOME/.gnupg/gpg.conf\"") {
		t.Errorf("script must `rm -f $HOME/.gnupg/gpg.conf` before importing to make re-runs work; got:\n%s", script)
	}

	// The import line and the ownertrust line must surface failures, not
	// silently swallow them with `2>/dev/null` AND `|| true`. We accept one
	// or the other for systemctl reload (which legitimately fans across
	// service names), but never both on a gpg operation.
	for _, line := range strings.Split(script, "\n") {
		isGPGImport := strings.Contains(line, "gpg --batch --import") ||
			strings.Contains(line, "gpg --import-ownertrust")
		if !isGPGImport {
			continue
		}
		if strings.Contains(line, "2>/dev/null") && strings.Contains(line, "|| true") {
			t.Errorf("gpg import/ownertrust line silences both stderr AND exit status, hiding failures: %q", line)
		}
	}
}

// TestBaseScriptMasksLocalGPGAgent guards a load-bearing precondition for
// every cloister VM that uses GPG forwarding: the local gpg-agent must be
// disabled so it cannot claim /run/user/<uid>/gnupg/S.gpg-agent — the path
// the cloister-managed reverse tunnel binds. The disable lives in base.sh
// (not the GPG-signing path) because base.sh runs early enough that no
// gpg invocation has had a chance to socket-activate the agent yet, which
// is what makes "never started" achievable instead of "started, then killed."
//
// The script also supports an UNMASK branch gated on CLOISTER_GPG_LOCAL=1
// (engine.go sets this when profile.GpgLocal is true) so users who want to
// manage GPG inside the VM can opt out of the mask. This test verifies both
// branches exist and that keyboxd.socket / dirmngr.socket are never masked
// (they are required by gpg --import even in the host-forwarding mode).
func TestBaseScriptMasksLocalGPGAgent(t *testing.T) {
	data, err := Scripts.ReadFile("scripts/base.sh")
	if err != nil {
		t.Fatalf("reading embedded scripts/base.sh: %v", err)
	}
	script := string(data)

	if !strings.Contains(script, "systemctl --user mask") {
		t.Fatalf("base.sh must `systemctl --user mask` the gpg-agent units (default branch); got:\n%s", script)
	}
	if !strings.Contains(script, "systemctl --user unmask") {
		t.Fatalf("base.sh must also `systemctl --user unmask` the gpg-agent units (gpg_local branch); got:\n%s", script)
	}
	if !strings.Contains(script, "CLOISTER_GPG_LOCAL") {
		t.Fatalf("base.sh must gate mask vs unmask on CLOISTER_GPG_LOCAL env var; got:\n%s", script)
	}

	for _, unit := range []string{"gpg-agent.socket", "gpg-agent-extra.socket", "gpg-agent-ssh.socket"} {
		if !strings.Contains(script, unit) {
			t.Errorf("base.sh must reference unit %q in mask/unmask commands; got:\n%s", unit, script)
		}
	}

	// keyboxd and dirmngr must not appear on a mask OR unmask line (we
	// leave them entirely untouched). Walk every mask/unmask line and
	// flag any forbidden unit reference.
	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "systemctl --user mask") && !strings.Contains(line, "systemctl --user unmask") {
			continue
		}
		for _, forbidden := range []string{"keyboxd.socket", "dirmngr.socket"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("base.sh must NOT touch %q (required by gpg --import); got line: %q", forbidden, line)
			}
		}
	}
}

// TestGitHubCLIBelongsToBaseProvisioning pins both ownership and failure
// policy: every profile gets gh from base, while the web stack does not carry
// a second installer. The repository is third-party, so failure must stay
// inside an if branch and remove its apt source rather than poisoning later
// apt-get updates under base.sh's set -e policy.
func TestGitHubCLIBelongsToBaseProvisioning(t *testing.T) {
	baseData, err := Scripts.ReadFile("scripts/base.sh")
	if err != nil {
		t.Fatalf("reading embedded scripts/base.sh: %v", err)
	}
	webData, err := Scripts.ReadFile("scripts/stack-web.sh")
	if err != nil {
		t.Fatalf("reading embedded scripts/stack-web.sh: %v", err)
	}
	base := string(baseData)
	web := string(webData)

	for _, want := range []string{
		`echo "=== Installing GitHub CLI ==="`,
		"https://cli.github.com/packages/githubcli-archive-keyring.gpg",
		"/etc/apt/sources.list.d/github-cli.list",
		"if sudo apt-get update -q && sudo apt-get install -y -q gh; then",
		"WARNING: the GitHub CLI package repository is unreachable",
		"github-cli.list.unreachable",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base.sh GitHub CLI installer missing %q", want)
		}
	}
	for _, duplicate := range []string{"cli.github.com", "github-cli-archive-keyring", "install -y -q gh"} {
		if strings.Contains(web, duplicate) {
			t.Errorf("stack-web.sh still owns GitHub CLI installation via %q", duplicate)
		}
	}

	preflight := strings.Index(base, "for list in /etc/apt/sources.list.d/*.list")
	install := strings.Index(base, `echo "=== Installing GitHub CLI ==="`)
	if preflight < 0 || install < 0 || preflight >= install {
		t.Errorf("GitHub CLI source must be re-added after the apt-source pre-flight (preflight=%d install=%d)", preflight, install)
	}
}

// TestAssembleScriptWithEnv verifies that assembleScriptWithEnv prepends the
// export line to the embedded script content and that the resulting string
// contains the expected script body.
func TestAssembleScriptWithEnv(t *testing.T) {
	script, err := assembleScriptWithEnv("scripts/read-only-mounts.sh", "CLOISTER_HEADLESS=1")
	if err != nil {
		t.Fatalf("assembleScriptWithEnv: %v", err)
	}
	if !strings.HasPrefix(script, "export CLOISTER_HEADLESS=1\n") {
		t.Error("assembled script should start with the export line")
	}
	if !strings.Contains(script, "READONLY_DIRS=") {
		t.Error("assembled script should contain read-only-mounts.sh body content")
	}
}

// renderBashrc executes the bashrc template with the given data and returns the
// rendered guest file.
func renderBashrc(t *testing.T, data bashrcTemplateData) string {
	t.Helper()
	raw, err := Templates.ReadFile("templates/bashrc.tmpl")
	if err != nil {
		t.Fatalf("reading bashrc.tmpl: %v", err)
	}
	tmpl, err := template.New("bashrc").Parse(string(raw))
	if err != nil {
		t.Fatalf("parsing bashrc.tmpl: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("executing bashrc.tmpl with data %+v: %v", data, err)
	}
	return buf.String()
}

// TestBashrcManagedWorkspaceDropsMountAliases verifies that a profile whose
// projects arrive as synchronized copies does not create ~/workspace or
// ~/code. Verified removal happens on the entry and repair paths, outside the
// login shell.
func TestBashrcManagedWorkspaceDropsMountAliases(t *testing.T) {
	t.Parallel()

	out := renderBashrc(t, bashrcTemplateData{
		Profile:          "work",
		StartDir:         "/Users/someone/Code/collection",
		ManagedWorkspace: true,
	})

	if strings.Contains(out, `ln -sfn "$WORKSPACE_EXPANDED" "$HOME/workspace"`) ||
		strings.Contains(out, `ln -sfn "$WORKSPACE_EXPANDED" "$HOME/code"`) {
		t.Errorf("managed-workspace bashrc still creates mount aliases; got:\n%s", out)
	}
	if strings.Contains(out, `rm -f "$stale_alias"`) {
		t.Errorf("managed-workspace bashrc performs unverified login-time cleanup; got:\n%s", out)
	}
	if !strings.Contains(out, `cd "$HOME/workspaces"`) {
		t.Errorf("managed-workspace bashrc does not default to ~/workspaces; got:\n%s", out)
	}
	if strings.Contains(out, `cd "/Users/someone/Code/collection"`) {
		t.Errorf("managed-workspace bashrc must not cd to the host start dir; got:\n%s", out)
	}
}

// TestBashrcMountedWorkspaceKeepsMountAliases pins the unchanged behavior for
// profiles that do reach their workspace through a host mount.
func TestBashrcMountedWorkspaceKeepsMountAliases(t *testing.T) {
	t.Parallel()

	out := renderBashrc(t, bashrcTemplateData{Profile: "dev", StartDir: "~/code"})

	if !strings.Contains(out, `ln -sfn "$WORKSPACE_EXPANDED" "$HOME/workspace"`) {
		t.Errorf("mounted-workspace bashrc lost its workspace alias; got:\n%s", out)
	}
	if !strings.Contains(out, `cd "~/code" 2>/dev/null || cd ~/workspace`) {
		t.Errorf("mounted-workspace bashrc lost its start-dir fallback; got:\n%s", out)
	}
}

// TestBashrcStartDirYieldsToTheSessionChoice verifies that the login-time cd is
// guarded. `cloister enter` changes directory before exec'ing this login shell,
// and an unguarded cd here silently discards that choice on every entry.
func TestBashrcStartDirYieldsToTheSessionChoice(t *testing.T) {
	t.Parallel()

	for _, data := range []bashrcTemplateData{
		{Profile: "dev", StartDir: "~/code"},
		{Profile: "work", StartDir: "~/code", ManagedWorkspace: true},
	} {
		out := renderBashrc(t, data)
		guard := `if [ "$PWD" = "$HOME" ]; then`
		guardAt := strings.Index(out, guard)
		if guardAt < 0 {
			t.Fatalf("bashrc has no start-dir guard for %+v; got:\n%s", data, out)
		}
		cdAt := strings.Index(out[guardAt:], "cd ")
		if cdAt < 0 {
			t.Fatalf("bashrc guard is not followed by a cd for %+v; got:\n%s", data, out)
		}
	}
}

// TestPruneWorkspaceAliasesSkipsMountedProfiles verifies that a profile whose
// workspace really is a host mount is left alone: on such a profile the
// ~/workspace and ~/code aliases are the working paths, not remnants.
func TestPruneWorkspaceAliasesSkipsMountedProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyRoot := filepath.Join(home, "host-workspace")
	codeAlias := filepath.Join(home, "code")
	if err := os.Symlink(legacyRoot, codeAlias); err != nil {
		t.Fatal(err)
	}
	backend := &vm.MockBackend{}
	engine := &Engine{}

	report, err := engine.PruneWorkspaceAliases("dev", &config.Profile{
		StartDir:  legacyRoot,
		Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeVirtiofs},
	}, backend)
	if err != nil {
		t.Fatalf("PruneWorkspaceAliases() error = %v", err)
	}
	if report.HasWarnings() {
		t.Errorf("report = %#v, want empty", report)
	}
	if len(backend.SSHScriptCalls) != 0 {
		t.Errorf("mounted profile ran %d guest scripts, want 0", len(backend.SSHScriptCalls))
	}
	if got, err := os.Readlink(codeAlias); err != nil || got != legacyRoot {
		t.Fatalf("mounted profile's legacy alias changed: target=%q err=%v", got, err)
	}
}

// localGuestBackend executes capture scripts against a temporary HOME. The
// embedded mock supplies every other vm.Backend method without reaching a VM.
type localGuestBackend struct {
	vm.MockBackend
	commandPrefix  string
	commandRewrite func(string) string
	captureRewrite func(string) string
}

func (b *localGuestBackend) SSHCapture(profile, script string) (string, error) {
	b.SSHScriptCalls = append(b.SSHScriptCalls, struct{ Profile, Script string }{profile, script})
	if b.captureRewrite != nil {
		script = b.captureRewrite(script)
	}
	script = linuxGuestToolShims(script)
	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("running guest script: %w: %s", err, out)
	}
	return string(out), nil
}

func (b *localGuestBackend) SSHCommand(profile, command string) (string, error) {
	b.SSHCommandCalls = append(b.SSHCommandCalls, struct{ Profile, Command string }{profile, command})
	if b.commandRewrite != nil {
		command = b.commandRewrite(command)
	}
	command = b.commandPrefix + linuxGuestToolShims(command)
	cmd := exec.Command("bash", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("running guest command: %w: %s", err, out)
	}
	return string(out), nil
}

func (b *localGuestBackend) SSHScriptTo(profile, script string, out io.Writer) (string, error) {
	b.SSHScriptCalls = append(b.SSHScriptCalls, struct{ Profile, Script string }{profile, script})
	script = linuxGuestToolShims(script)
	cmd := exec.Command("bash", "-c", script)
	guest, err := cmd.CombinedOutput()
	if out != nil {
		_, _ = out.Write(guest)
	}
	if err != nil {
		return string(guest), fmt.Errorf("running guest script: %w: %s", err, guest)
	}
	return string(guest), nil
}

// linuxGuestToolShims selects GNU utilities on macOS, matching the Linux guest
// where the generated scripts run in production.
func linuxGuestToolShims(script string) string {
	var shims strings.Builder
	for _, tool := range []string{"realpath", "readlink", "mv", "mktemp"} {
		gnuTool, err := exec.LookPath("g" + tool)
		if err != nil {
			continue
		}
		fmt.Fprintf(&shims, "%s() { %s \"$@\"; }\n", tool, shellSingleQuote(gnuTool))
	}
	if _, err := exec.LookPath("flock"); err != nil {
		// The generated script runs in Linux, where util-linux provides flock.
		// Local macOS tests are single-process unless they inspect the script.
		shims.WriteString("flock() { :; }\n")
	}
	return shims.String() + script
}

// cleanupToolFaultRewrite replaces one selected invocation while delegating
// every other invocation to the same GNU implementation selected by
// linuxGuestToolShims.
func cleanupToolFaultRewrite(t *testing.T, tool string, call int, action string) func(string) string {
	t.Helper()
	toolPath, err := exec.LookPath("g" + tool)
	if err != nil {
		toolPath, err = exec.LookPath(tool)
	}
	if err != nil {
		t.Skipf("%s is unavailable", tool)
	}
	action = strings.ReplaceAll(action, "__CLOISTER_ACTUAL_TOOL__", shellSingleQuote(toolPath))
	counter := "cloister_test_" + tool + "_calls"
	condition := fmt.Sprintf(`[ "$%s" -eq %d ]`, counter, call)
	if call < 0 {
		condition = fmt.Sprintf(`[ "$%s" -ge %d ]`, counter, -call)
	}
	wrapper := fmt.Sprintf(`%s=0
%s() {
	%s=$((%s + 1))
	if %s; then
		%s
	fi
	%s "$@"
}
`, counter, tool, counter, counter, condition, action, shellSingleQuote(toolPath))
	return func(script string) string {
		const insertion = "export LC_ALL\n"
		if !strings.Contains(script, insertion) {
			t.Fatal("cleanup fault injection did not find the script preamble")
		}
		return strings.Replace(script, insertion, insertion+wrapper, 1)
	}
}

func managedWorkspaceProfile(mode, legacyRoot, managedRoot string) *config.Profile {
	return &config.Profile{
		StartDir: legacyRoot,
		Workspace: config.WorkspaceConfig{
			Mode: mode,
			Root: managedRoot,
		},
	}
}

func writeAliasQuarantineMarker(t *testing.T, dir, name, target string) {
	t.Helper()
	// Both fields use NUL terminators, matching the generated marker exactly
	// without making either value pass through line-oriented shell parsing.
	if err := os.WriteFile(filepath.Join(dir, "."+name+".cloister-marker"), []byte(name+"\x00"+target+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureBashrcRedeploysAfterWorkspaceModeChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyRoot := filepath.Join(home, "host-workspace")
	backend := &localGuestBackend{}
	engine := &Engine{}
	virtiofs := &config.Profile{
		StartDir:  legacyRoot,
		Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeVirtiofs},
	}
	if err := engine.DeployBashrc("work", virtiofs, backend); err != nil {
		t.Fatal(err)
	}
	backend.SSHCommandCalls = nil
	managed := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

	result, err := engine.EnsureBashrc("work", managed, backend)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("workspace-mode change did not redeploy the stale bashrc")
	}
	if len(backend.SSHCommandCalls) != 1 {
		t.Fatalf("bashrc deployments = %d, want 1", len(backend.SSHCommandCalls))
	}
	deployed, err := os.ReadFile(filepath.Join(home, ".bashrc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(deployed), `ln -sfn "$WORKSPACE_EXPANDED" "$HOME/code"`) {
		t.Fatalf("redeployed bashrc still has the virtiofs alias branch:\n%s", deployed)
	}
}

func TestEnsureBashrcSkipsMatchingManagedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	backend := &localGuestBackend{}
	engine := &Engine{}
	profile := managedWorkspaceProfile(
		config.WorkspaceModeWorkspace,
		filepath.Join(home, "host-workspace"),
		filepath.Join(home, "workspaces"),
	)
	if err := engine.DeployBashrc("work", profile, backend); err != nil {
		t.Fatal(err)
	}
	backend.SSHCommandCalls = nil
	backend.SSHScriptCalls = nil

	result, err := engine.EnsureBashrc("work", profile, backend)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed {
		t.Fatal("matching bashrc was reported as redeployed")
	}
	if len(backend.SSHCommandCalls) != 0 {
		t.Fatalf("matching bashrc caused %d deployment(s), want 0", len(backend.SSHCommandCalls))
	}
	if len(backend.SSHScriptCalls) != 1 {
		t.Fatalf("digest checks = %d, want 1", len(backend.SSHScriptCalls))
	}
}

func TestEnsureBashrcThenCleanupLeavesNoLegacyAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyRoot := filepath.Join(home, "host-workspace")
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "workspaces"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &localGuestBackend{}
	engine := &Engine{}
	virtiofs := &config.Profile{
		StartDir:  legacyRoot,
		Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeVirtiofs},
	}
	if err := engine.DeployBashrc("work", virtiofs, backend); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"code", "workspace"} {
		if err := os.Symlink(legacyRoot, filepath.Join(home, name)); err != nil {
			t.Fatal(err)
		}
	}
	managed := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

	result, err := engine.EnsureBashrc("work", managed, backend)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("stale bashrc was not redeployed")
	}
	if _, err := engine.PruneWorkspaceAliases("work", managed, backend); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"code", "workspace"} {
		if _, err := os.Lstat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy alias %q remains after redeploy and cleanup: %v", name, err)
		}
	}
}

func TestEnsureBashrcReplacesSymlinkWithoutTouchingTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "dotfiles", "bashrc")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	const original = "user-managed dotfile\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".bashrc")); err != nil {
		t.Fatal(err)
	}
	backend := &localGuestBackend{}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	result, err := (&Engine{}).EnsureBashrc("work", profile, backend)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.ReplacedSymlink {
		t.Fatalf("result = %#v, want changed symlink replacement", result)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != original {
		t.Fatalf("symlink target changed: contents=%q err=%v", got, err)
	}
	info, err := os.Lstat(filepath.Join(home, ".bashrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("deployed ~/.bashrc mode = %s, want regular file", info.Mode())
	}
}

func TestEnsureBashrcReplacesSymlinkEvenWhenTargetContentMatches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))
	rendered, err := renderTemplate("templates/bashrc.tmpl", bashrcData("work", profile))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "dotfiles-bashrc")
	original := []byte(rendered + "\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".bashrc")); err != nil {
		t.Fatal(err)
	}

	result, err := (&Engine{}).EnsureBashrc("work", profile, &localGuestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.ReplacedSymlink {
		t.Fatalf("matching symlink result = %#v, want replacement", result)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("matching symlink target changed: size=%d err=%v", len(got), err)
	}
}

func TestEnsureBashrcBreaksHardlinkWithoutTouchingPeer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	peer := filepath.Join(home, "user-file")
	const original = "shared inode content\n"
	if err := os.WriteFile(peer, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	bashrc := filepath.Join(home, ".bashrc")
	if err := os.Link(peer, bashrc); err != nil {
		t.Fatal(err)
	}
	backend := &localGuestBackend{}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	result, err := (&Engine{}).EnsureBashrc("work", profile, backend)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("hardlinked stale bashrc was not replaced")
	}
	if got, err := os.ReadFile(peer); err != nil || string(got) != original {
		t.Fatalf("hardlink peer changed: contents=%q err=%v", got, err)
	}
	peerInfo, err := os.Stat(peer)
	if err != nil {
		t.Fatal(err)
	}
	bashrcInfo, err := os.Stat(bashrc)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(peerInfo, bashrcInfo) {
		t.Fatal("deployed ~/.bashrc still shares the user's hardlink inode")
	}
}

func TestEnsureBashrcWriteFailureLeavesOriginalIntact(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bashrc := filepath.Join(home, ".bashrc")
	original := bytes.Repeat([]byte("user bashrc data\n"), 160)
	if err := os.WriteFile(bashrc, original, 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &localGuestBackend{commandPrefix: "ulimit -f 1\n"}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	if _, err := (&Engine{}).EnsureBashrc("work", profile, backend); err == nil {
		t.Fatal("mid-write failure returned nil error")
	}
	if got, err := os.ReadFile(bashrc); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("failed deployment changed original: size=%d err=%v", len(got), err)
	}
	temps, err := filepath.Glob(filepath.Join(home, ".bashrc.cloister-tmp.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("failed deployment left temporary files: %v", temps)
	}
}

func TestEnsureBashrcPreRenameFailureCleansTempAndKeepsOriginal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bashrc := filepath.Join(home, ".bashrc")
	original := []byte("original bashrc\n")
	if err := os.WriteFile(bashrc, original, 0o600); err != nil {
		t.Fatal(err)
	}
	injected := false
	backend := &localGuestBackend{commandRewrite: func(command string) string {
		needle := "CLOISTER_EOF\nchmod 0600 \"$tmp\""
		if strings.Contains(command, needle) {
			injected = true
			return strings.Replace(command, needle, "CLOISTER_EOF\nfalse\nchmod 0600 \"$tmp\"", 1)
		}
		return command
	}}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	if _, err := (&Engine{}).EnsureBashrc("work", profile, backend); err == nil {
		t.Fatal("pre-rename failure returned nil error")
	}
	if !injected {
		t.Fatal("failure injection did not reach the pre-rename seam")
	}
	if got, err := os.ReadFile(bashrc); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("pre-rename failure changed original: contents=%q err=%v", got, err)
	}
	temps, err := filepath.Glob(filepath.Join(home, ".bashrc.cloister-tmp.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("pre-rename failure left temporary files: %v", temps)
	}
}

func TestEnsureBashrcRedeploysUnreadableFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bashrc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bashrc, []byte("stale\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	backend := &localGuestBackend{}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	result, err := (&Engine{}).EnsureBashrc("work", profile, backend)
	if err != nil {
		t.Fatalf("unreadable bashrc blocked reconciliation: %v", err)
	}
	if !result.Changed {
		t.Fatal("unreadable bashrc was not redeployed")
	}
	if got, err := os.ReadFile(bashrc); err != nil || !strings.Contains(string(got), "cloister-managed bashrc") {
		t.Fatalf("redeployed bashrc = %q, err=%v", got, err)
	}
}

func TestEnsureBashrcTransportFailureDoesNotDeploy(t *testing.T) {
	backend := &vm.MockBackend{SSHScriptErr: errors.New("transport unavailable")}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, "/host/workspace", "/host/workspaces")

	_, err := (&Engine{}).EnsureBashrc("work", profile, backend)
	if err == nil || !strings.Contains(err.Error(), "transport unavailable") {
		t.Fatalf("EnsureBashrc() error = %v, want transport failure", err)
	}
	if len(backend.SSHCommandCalls) != 0 {
		t.Fatalf("transport failure attempted %d blind deployment(s)", len(backend.SSHCommandCalls))
	}
}

func TestDeployVMConfigReplacesSymlinkWithoutTouchingTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".cloister-vm")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "user-config")
	const original = "user-owned config\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(configDir, "config.json")
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	backend := &localGuestBackend{}

	if err := (&Engine{}).DeployVMConfig("work", &config.Profile{}, backend, nil, "~/code"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != original {
		t.Fatalf("VM config symlink target changed: contents=%q err=%v", got, err)
	}
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("deployed VM config mode = %s, want regular file", info.Mode())
	}
}

func TestPruneWorkspaceAliasesRestoresObjectSwappedAfterLstat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyRoot := filepath.Join(home, "host-workspace")
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	code := filepath.Join(home, "code")
	if err := os.Symlink(legacyRoot, code); err != nil {
		t.Fatal(err)
	}
	const userData = "created during cleanup\n"
	injected := false
	backend := &localGuestBackend{captureRewrite: func(script string) string {
		needle := "    if [ -L \"$alias\" ]; then\n\t\tname="
		replacement := "    if [ -L \"$alias\" ]; then\n" +
			"\t\tif [ \"${alias##*/}\" = code ]; then unlink -- \"$alias\"; printf '" + userData + "' > \"$alias\"; fi\n" +
			"\t\tname="
		if strings.Contains(script, needle) {
			injected = true
			return strings.Replace(script, needle, replacement, 1)
		}
		return script
	}}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

	report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
	if err != nil {
		t.Fatal(err)
	}
	if !injected {
		t.Fatal("race injection did not reach the post-lstat seam")
	}
	if len(report.PreservedAliases) != 1 || report.PreservedAliases[0] != "~/code" {
		t.Fatalf("preserved aliases = %#v, want [~/code]", report.PreservedAliases)
	}
	if got, err := os.ReadFile(code); err != nil || string(got) != userData {
		t.Fatalf("swapped regular file was not restored: contents=%q err=%v", got, err)
	}
}

func TestPruneWorkspaceAliasesRecoversAfterCleanupIsKilled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyRoot := filepath.Join(home, "host-workspace")
	userTarget := filepath.Join(home, "personal-projects")
	code := filepath.Join(home, "code")
	if err := os.Symlink(userTarget, code); err != nil {
		t.Fatal(err)
	}
	killed := false
	backend := &localGuestBackend{captureRewrite: func(script string) string {
		needle := "\t\tfi\n\t\tpreserved=0\n"
		if strings.Contains(script, needle) {
			killed = true
			return strings.Replace(script, needle, "\t\tfi\n\t\tkill -KILL $$\n\t\tpreserved=0\n", 1)
		}
		return script
	}}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

	if _, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend); err == nil {
		t.Fatal("SIGKILL-injected cleanup returned nil error")
	}
	if !killed {
		t.Fatal("SIGKILL injection did not reach the post-rename seam")
	}
	if _, err := os.Lstat(code); !os.IsNotExist(err) {
		t.Fatalf("interrupted cleanup left ~/code present: %v", err)
	}
	stranded, err := filepath.Glob(filepath.Join(home, ".cloister-alias-quarantine.*", "code"))
	if err != nil || len(stranded) != 1 {
		t.Fatalf("stranded entries = %v, err=%v, want one", stranded, err)
	}

	report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, &localGuestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if report.HasWarnings() {
		t.Fatalf("recovery report = %#v, want clean restoration", report)
	}
	if got, err := os.Readlink(code); err != nil || got != userTarget {
		t.Fatalf("recovered ~/code target = %q, err=%v", got, err)
	}
	dirs, err := filepath.Glob(filepath.Join(home, ".cloister-alias-quarantine.*"))
	if err != nil || len(dirs) != 0 {
		t.Fatalf("empty quarantine directories remain: %v, err=%v", dirs, err)
	}
}

func TestPruneWorkspaceAliasesMarkerFirstCrashLeavesAliasInHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userTarget := filepath.Join(home, "personal-projects")
	code := filepath.Join(home, "code")
	if err := os.Symlink(userTarget, code); err != nil {
		t.Fatal(err)
	}
	killed := false
	backend := &localGuestBackend{captureRewrite: func(script string) string {
		needle := "\t\t# Moving the directory entry first closes the check/remove race. Every\n"
		if strings.Contains(script, needle) {
			killed = true
			return strings.Replace(script, needle, "\t\tkill -KILL $$\n"+needle, 1)
		}
		return script
	}}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	if _, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend); err == nil {
		t.Fatal("marker-first SIGKILL returned nil error")
	}
	if !killed {
		t.Fatal("SIGKILL injection did not reach the marker-before-rename seam")
	}
	if got, err := os.Readlink(code); err != nil || got != userTarget {
		t.Fatalf("marker-first crash displaced alias: target=%q err=%v", got, err)
	}
	markers, err := filepath.Glob(filepath.Join(home, ".cloister-alias-quarantine.*", ".code.cloister-marker"))
	if err != nil || len(markers) != 1 {
		t.Fatalf("orphan markers = %v, err=%v, want one", markers, err)
	}

	if _, err := (&Engine{}).PruneWorkspaceAliases("work", profile, &localGuestBackend{}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(code); err != nil || got != userTarget {
		t.Fatalf("later cleanup changed user alias: target=%q err=%v", got, err)
	}
	dirs, err := filepath.Glob(filepath.Join(home, ".cloister-alias-quarantine.*"))
	if err != nil || len(dirs) != 0 {
		t.Fatalf("orphan marker quarantine remains: %v, err=%v", dirs, err)
	}
}

func TestPruneWorkspaceAliasesRecoversSeveralQuarantinesWithoutOverwrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codeTarget := filepath.Join(home, "personal-code")
	workspaceTarget := filepath.Join(home, "personal-workspace")
	codeQuarantine := filepath.Join(home, ".cloister-alias-quarantine.A1b2C3")
	workspaceQuarantine := filepath.Join(home, ".cloister-alias-quarantine.D4e5F6")
	for _, dir := range []string{codeQuarantine, workspaceQuarantine} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(codeTarget, filepath.Join(codeQuarantine, "code")); err != nil {
		t.Fatal(err)
	}
	writeAliasQuarantineMarker(t, codeQuarantine, "code", codeTarget)
	if err := os.Symlink(workspaceTarget, filepath.Join(workspaceQuarantine, "workspace")); err != nil {
		t.Fatal(err)
	}
	writeAliasQuarantineMarker(t, workspaceQuarantine, "workspace", workspaceTarget)
	workspace := filepath.Join(home, "workspace")
	const occupant = "do not overwrite\n"
	if err := os.WriteFile(workspace, []byte(occupant), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, &localGuestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(home, "code")); err != nil || got != codeTarget {
		t.Fatalf("recovered ~/code target = %q, err=%v", got, err)
	}
	if got, err := os.ReadFile(workspace); err != nil || string(got) != occupant {
		t.Fatalf("occupied ~/workspace changed: contents=%q err=%v", got, err)
	}
	wantStranded := "~/.cloister-alias-quarantine.D4e5F6/workspace"
	if len(report.StrandedAliases) != 1 || report.StrandedAliases[0] != wantStranded {
		t.Fatalf("stranded aliases = %#v, want [%s]", report.StrandedAliases, wantStranded)
	}
	if _, err := os.Stat(codeQuarantine); !os.IsNotExist(err) {
		t.Fatalf("empty recovered quarantine remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspaceQuarantine, "workspace")); err != nil {
		t.Fatalf("occupied entry was not preserved in quarantine: %v", err)
	}

	if err := os.Remove(workspace); err != nil {
		t.Fatal(err)
	}
	report, err = (&Engine{}).PruneWorkspaceAliases("work", profile, &localGuestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StrandedAliases) != 0 {
		t.Fatalf("second recovery still reports stranded entries: %#v", report.StrandedAliases)
	}
	if got, err := os.Readlink(workspace); err != nil || got != workspaceTarget {
		t.Fatalf("second-run recovered ~/workspace target = %q, err=%v", got, err)
	}
	if _, err := os.Stat(workspaceQuarantine); !os.IsNotExist(err) {
		t.Fatalf("quarantine remains after becoming empty: %v", err)
	}
}

func TestPruneWorkspaceAliasesRefusesUnverifiedQuarantineEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	type quarantinedEntry struct {
		dir    string
		assert func(*testing.T, string)
	}
	var entries []quarantinedEntry
	makeDir := func(suffix string) string {
		dir := filepath.Join(home, ".cloister-alias-quarantine."+suffix)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	regularDir := makeDir("Reg123")
	regular := filepath.Join(regularDir, "code")
	if err := os.WriteFile(regular, []byte("user file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAliasQuarantineMarker(t, regularDir, "code", "user file")
	entries = append(entries, quarantinedEntry{regularDir, func(t *testing.T, path string) {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "user file\n" {
			t.Fatalf("regular quarantine entry changed: contents=%q err=%v", got, err)
		}
	}})

	directoryDir := makeDir("Dir123")
	directory := filepath.Join(directoryDir, "code", "nested")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "sentinel"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAliasQuarantineMarker(t, directoryDir, "code", "not-a-symlink")
	entries = append(entries, quarantinedEntry{directoryDir, func(t *testing.T, path string) {
		got, err := os.ReadFile(filepath.Join(path, "nested", "sentinel"))
		if err != nil || string(got) != "keep" {
			t.Fatalf("directory quarantine entry changed: contents=%q err=%v", got, err)
		}
	}})

	missingMarkerDir := makeDir("NoM123")
	missingTarget := filepath.Join(home, "missing-marker-target")
	if err := os.Symlink(missingTarget, filepath.Join(missingMarkerDir, "code")); err != nil {
		t.Fatal(err)
	}
	entries = append(entries, quarantinedEntry{missingMarkerDir, func(t *testing.T, path string) {
		if got, err := os.Readlink(path); err != nil || got != missingTarget {
			t.Fatalf("markerless symlink changed: target=%q err=%v", got, err)
		}
	}})

	symlinkMarkerDir := makeDir("Sym123")
	symlinkMarkerTarget := filepath.Join(home, "symlink-marker-target")
	if err := os.Symlink(symlinkMarkerTarget, filepath.Join(symlinkMarkerDir, "code")); err != nil {
		t.Fatal(err)
	}
	markerContents := filepath.Join(home, "marker-contents")
	if err := os.WriteFile(markerContents, []byte("code\x00"+symlinkMarkerTarget+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(markerContents, filepath.Join(symlinkMarkerDir, ".code.cloister-marker")); err != nil {
		t.Fatal(err)
	}
	entries = append(entries, quarantinedEntry{symlinkMarkerDir, func(t *testing.T, path string) {
		if got, err := os.Readlink(path); err != nil || got != symlinkMarkerTarget {
			t.Fatalf("entry with symlinked marker changed: target=%q err=%v", got, err)
		}
	}})

	mismatchDir := makeDir("Mis123")
	mismatchTarget := filepath.Join(home, "actual-target")
	if err := os.Symlink(mismatchTarget, filepath.Join(mismatchDir, "code")); err != nil {
		t.Fatal(err)
	}
	writeAliasQuarantineMarker(t, mismatchDir, "code", filepath.Join(home, "different-target"))
	entries = append(entries, quarantinedEntry{mismatchDir, func(t *testing.T, path string) {
		if got, err := os.Readlink(path); err != nil || got != mismatchTarget {
			t.Fatalf("marker-mismatched symlink changed: target=%q err=%v", got, err)
		}
	}})

	wrongNameDir := makeDir("Nam123")
	wrongNameTarget := filepath.Join(home, "wrong-name-target")
	if err := os.Symlink(wrongNameTarget, filepath.Join(wrongNameDir, "code")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrongNameDir, ".code.cloister-marker"), []byte("workspace\x00"+wrongNameTarget+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries = append(entries, quarantinedEntry{wrongNameDir, func(t *testing.T, path string) {
		if got, err := os.Readlink(path); err != nil || got != wrongNameTarget {
			t.Fatalf("wrong-name marker entry changed: target=%q err=%v", got, err)
		}
	}})

	nulNameDir := makeDir("Nul123")
	nulNameTarget := filepath.Join(home, "nul-name-target")
	if err := os.Symlink(nulNameTarget, filepath.Join(nulNameDir, "code")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nulNameDir, ".code.cloister-marker"), []byte("co\x00de\x00"+nulNameTarget+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries = append(entries, quarantinedEntry{nulNameDir, func(t *testing.T, path string) {
		if got, err := os.Readlink(path); err != nil || got != nulNameTarget {
			t.Fatalf("NUL-containing marker-name entry changed: target=%q err=%v", got, err)
		}
	}})

	orphanDir := makeDir("Orp123")
	orphanMarker := filepath.Join(orphanDir, ".code.cloister-marker")
	if err := os.WriteFile(orphanMarker, []byte("code\x00foreign-target\x00"), 0o600); err != nil {
		t.Fatal(err)
	}

	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))
	report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, &localGuestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.UnverifiedQuarantineEntries) != len(entries) {
		t.Fatalf("unverified entries = %#v, want %d", report.UnverifiedQuarantineEntries, len(entries))
	}
	if _, err := os.Lstat(filepath.Join(home, "code")); !os.IsNotExist(err) {
		t.Fatalf("unverified entry was moved into ~/code: %v", err)
	}
	for _, entry := range entries {
		entry.assert(t, filepath.Join(entry.dir, "code"))
	}
	if got, err := os.ReadFile(orphanMarker); err != nil || string(got) != "code\x00foreign-target\x00" {
		t.Fatalf("unmatched orphan marker changed: contents=%q err=%v", got, err)
	}
}

func TestPruneWorkspaceAliasesLockWaitIsBounded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOISTER_TEST_FLOCK_HELPER", "1")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	flockStub := "#!/bin/sh\nexec " + shellSingleQuote(testBinary) + " -test.run=TestFlockHelperProcess -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "flock"), []byte(flockStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	holder := exec.Command(testBinary, "-test.run=TestFlockHelperProcess", "--", "holder", home)
	holderOut, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	ready, err := bufio.NewReader(holderOut).ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatalf("lock holder readiness = %q, err=%v", ready, err)
	}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	// Start timing only after the separate holder confirms that it owns the
	// lock. There is deliberately no wall-clock ceiling: an unbounded waiter
	// proceeds only after the holder exits and therefore returns success, which
	// fails the assertion below without making scheduler delay look like a flake.
	started := time.Now()
	_, err = (&Engine{}).PruneWorkspaceAliases("work", profile, &localGuestBackend{})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "skipped") {
		t.Fatalf("lock timeout error = %v, want explicit skipped-cleanup error", err)
	}
	if elapsed < time.Duration(workspaceCleanupLockWaitSeconds)*time.Second-time.Second/2 {
		t.Fatalf("lock wait returned too early after %s", elapsed)
	}
}

// TestFlockHelperProcess is re-executed as a test-only flock command so the
// bounded contention test exercises a real kernel lock on hosts where the
// util-linux executable is unavailable.
func TestFlockHelperProcess(t *testing.T) {
	if os.Getenv("CLOISTER_TEST_FLOCK_HELPER") != "1" {
		return
	}
	args := os.Args
	separator := -1
	for index, arg := range args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		os.Exit(64)
	}
	args = args[separator+1:]
	if len(args) == 2 && args[0] == "holder" {
		home, err := os.Open(args[1])
		if err != nil {
			os.Exit(66)
		}
		if err := syscall.Flock(int(home.Fd()), syscall.LOCK_EX); err != nil {
			os.Exit(70)
		}
		_, _ = fmt.Fprintln(os.Stdout, "ready")
		time.Sleep(15 * time.Second)
		os.Exit(0)
	}
	waitSeconds := 0
	conflictExit := 1
	fd := -1
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-w":
			index++
			if index >= len(args) {
				os.Exit(64)
			}
			waitSeconds, _ = strconv.Atoi(args[index])
		case "-E":
			index++
			if index >= len(args) {
				os.Exit(64)
			}
			conflictExit, _ = strconv.Atoi(args[index])
		default:
			fd, _ = strconv.Atoi(args[index])
		}
	}
	if waitSeconds <= 0 || conflictExit <= 0 || fd < 0 {
		os.Exit(64)
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			os.Exit(0)
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			os.Exit(70)
		}
		if !time.Now().Before(deadline) {
			os.Exit(conflictExit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPruneWorkspaceAliasesIgnoresQuarantineLookalikes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	lookalike := filepath.Join(home, ".cloister-alias-quarantine.not-cloister")
	if err := os.Mkdir(lookalike, 0o700); err != nil {
		t.Fatal(err)
	}
	userTarget := filepath.Join(home, "personal-code")
	if err := os.Symlink(userTarget, filepath.Join(lookalike, "code")); err != nil {
		t.Fatal(err)
	}
	externalDir := filepath.Join(home, "external")
	if err := os.Mkdir(externalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(userTarget, filepath.Join(externalDir, "code")); err != nil {
		t.Fatal(err)
	}
	symlinkLookalike := filepath.Join(home, ".cloister-alias-quarantine.A1b2C3")
	if err := os.Symlink(externalDir, symlinkLookalike); err != nil {
		t.Fatal(err)
	}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	if _, err := (&Engine{}).PruneWorkspaceAliases("work", profile, &localGuestBackend{}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(lookalike, "code")); err != nil || got != userTarget {
		t.Fatalf("lookalike quarantine was modified: target=%q err=%v", got, err)
	}
	if got, err := os.Readlink(filepath.Join(externalDir, "code")); err != nil || got != userTarget {
		t.Fatalf("symlinked quarantine target was modified: target=%q err=%v", got, err)
	}
	if got, err := os.Readlink(symlinkLookalike); err != nil || got != externalDir {
		t.Fatalf("quarantine-shaped symlink was modified: target=%q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(home, "code")); !os.IsNotExist(err) {
		t.Fatalf("lookalike entry was restored into guest home: %v", err)
	}
}

func TestPruneWorkspaceAliasesRemovesOnlyLegacyLinksAndIsIdempotent(t *testing.T) {
	for _, mode := range []string{config.WorkspaceModeWorkspace, config.WorkspaceModeBroker} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			legacyRoot := filepath.Join(home, "host", "Code", "collection")
			if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			workspacesSentinel := filepath.Join(home, "workspaces", "project", "sentinel")
			if err := os.MkdirAll(filepath.Dir(workspacesSentinel), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(workspacesSentinel, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"code", "workspace"} {
				if err := os.Symlink(legacyRoot, filepath.Join(home, name)); err != nil {
					t.Fatal(err)
				}
			}

			backend := &localGuestBackend{}
			profile := managedWorkspaceProfile(mode, legacyRoot, filepath.Join(home, "workspaces"))
			report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
			if err != nil {
				t.Fatalf("first cleanup: %v", err)
			}
			if report.HasWarnings() {
				t.Errorf("first report = %#v, want empty", report)
			}
			for _, name := range []string{"code", "workspace"} {
				if _, err := os.Lstat(filepath.Join(home, name)); !os.IsNotExist(err) {
					t.Errorf("legacy alias %q remains: %v", name, err)
				}
			}
			if got, err := os.ReadFile(workspacesSentinel); err != nil || string(got) != "keep" {
				t.Fatalf("~/workspaces was modified: contents=%q err=%v", got, err)
			}

			report, err = (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
			if err != nil {
				t.Fatalf("second cleanup: %v", err)
			}
			if report.HasWarnings() {
				t.Errorf("second report = %#v, want clean no-op", report)
			}
			if got, err := os.ReadFile(workspacesSentinel); err != nil || string(got) != "keep" {
				t.Fatalf("second cleanup modified ~/workspaces: contents=%q err=%v", got, err)
			}
		})
	}
}

func TestPruneWorkspaceAliasesPreservesRealDirectoryWithWarning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codeFile := filepath.Join(home, "code", "sentinel")
	if err := os.MkdirAll(filepath.Dir(codeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codeFile, []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyRoot := filepath.Join(home, "host-workspace")
	backend := &localGuestBackend{}

	report, err := (&Engine{}).PruneWorkspaceAliases("work", managedWorkspaceProfile(
		config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "managed-root-not-mounted"),
	), backend)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.PreservedAliases) != 1 || report.PreservedAliases[0] != "~/code" {
		t.Fatalf("preserved aliases = %#v, want [~/code]", report.PreservedAliases)
	}
	if got, err := os.ReadFile(codeFile); err != nil || string(got) != "user data" {
		t.Fatalf("real ~/code directory was modified: contents=%q err=%v", got, err)
	}
}

func TestPruneWorkspaceAliasesPreservesUserSymlinkOutsideMount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyRoot := filepath.Join(home, "host-workspace")
	userTarget := filepath.Join(home, "personal-projects")
	if err := os.Symlink(userTarget, filepath.Join(home, "code")); err != nil {
		t.Fatal(err)
	}
	backend := &localGuestBackend{}

	report, err := (&Engine{}).PruneWorkspaceAliases("work", managedWorkspaceProfile(
		config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "managed-root-not-mounted"),
	), backend)
	if err != nil {
		t.Fatal(err)
	}
	if report.HasWarnings() {
		t.Errorf("report = %#v, want unrelated symlink silently preserved", report)
	}
	if got, err := os.Readlink(filepath.Join(home, "code")); err != nil || got != userTarget {
		t.Fatalf("user symlink changed: target=%q err=%v", got, err)
	}
}

func TestPruneWorkspaceAliasesPreservesNewlineBearingTargets(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		target func(home, legacy string) string
	}{
		{name: "one trailing newline", target: func(_, legacy string) string { return legacy + "\n" }},
		{name: "multiple trailing newlines", target: func(_, legacy string) string { return legacy + "\n\n" }},
		{name: "embedded newline", target: func(home, _ string) string { return filepath.Join(home, "host\nworkspace") }},
	} {
		for _, source := range []string{"direct", "recovered"} {
			t.Run(testCase.name+"/"+source, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				legacyRoot := filepath.Join(home, "host-workspace")
				target := testCase.target(home, legacyRoot)
				code := filepath.Join(home, "code")
				if source == "direct" {
					if err := os.Symlink(target, code); err != nil {
						t.Fatal(err)
					}
				} else {
					quarantine := filepath.Join(home, ".cloister-alias-quarantine.Nl1234")
					if err := os.Mkdir(quarantine, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, filepath.Join(quarantine, "code")); err != nil {
						t.Fatal(err)
					}
					writeAliasQuarantineMarker(t, quarantine, "code", target)
				}
				profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

				if _, err := (&Engine{}).PruneWorkspaceAliases("work", profile, &localGuestBackend{}); err != nil {
					t.Fatal(err)
				}
				if got, err := os.Readlink(code); err != nil || got != target {
					t.Fatalf("newline-bearing symlink was removed or changed: target=%q want=%q err=%v", got, target, err)
				}
			})
		}
	}
}

func TestPruneWorkspaceAliasesReadlinkFailuresAtEveryDirectUseFailClosed(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		call   int
		action string
	}{
		{name: "marker creation status", call: 2, action: `__CLOISTER_ACTUAL_TOOL__ "$@"; return 70`},
		{name: "marker creation empty", call: 2, action: "return 0"},
		{name: "post-move marker validation", call: 3, action: `__CLOISTER_ACTUAL_TOOL__ "$@"; return 70`},
		{name: "target acquisition", call: 4, action: `__CLOISTER_ACTUAL_TOOL__ "$@"; return 70`},
		{name: "short target acquisition", call: 4, action: "printf '/short'; return 0"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			legacyRoot := filepath.Join(home, "host-workspace")
			code := filepath.Join(home, "code")
			if err := os.Symlink(legacyRoot, code); err != nil {
				t.Fatal(err)
			}
			backend := &localGuestBackend{captureRewrite: cleanupToolFaultRewrite(t, "readlink", testCase.call, testCase.action)}
			profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

			report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(report.UnverifiedAliases, []string{"~/code"}) {
				t.Fatalf("unverified aliases = %#v, want [~/code]", report.UnverifiedAliases)
			}
			if got, err := os.Readlink(code); err != nil || got != legacyRoot {
				t.Fatalf("producer failure changed legacy alias: target=%q err=%v", got, err)
			}
		})
	}
}

func TestPruneWorkspaceAliasesMarkerSerializationFailuresFailClosed(t *testing.T) {
	for _, testCase := range []struct {
		tool   string
		action string
	}{
		{tool: "cat", action: "return 70"},
		{tool: "wc", action: `__CLOISTER_ACTUAL_TOOL__ "$@"; return 70`},
	} {
		t.Run(testCase.tool, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			legacyRoot := filepath.Join(home, "host-workspace")
			code := filepath.Join(home, "code")
			if err := os.Symlink(legacyRoot, code); err != nil {
				t.Fatal(err)
			}
			backend := &localGuestBackend{captureRewrite: cleanupToolFaultRewrite(t, testCase.tool, 1, testCase.action)}
			profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

			report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(report.UnverifiedAliases, []string{"~/code"}) {
				t.Fatalf("unverified aliases = %#v, want [~/code]", report.UnverifiedAliases)
			}
			if got, err := os.Readlink(code); err != nil || got != legacyRoot {
				t.Fatalf("serialization failure changed alias: target=%q err=%v", got, err)
			}
		})
	}
}

func TestPruneWorkspaceAliasesRecoveryProducerFailuresFailClosed(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		action       string
		markerTarget string
	}{
		{name: "failed readlink", action: `__CLOISTER_ACTUAL_TOOL__ "$@"; return 70`, markerTarget: "personal-code"},
		{name: "empty successful readlink", action: "return 0"},
		{name: "short successful readlink", action: "printf '/short'; return 0"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			quarantine := filepath.Join(home, ".cloister-alias-quarantine.Fail01")
			if err := os.Mkdir(quarantine, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(home, "personal-code")
			held := filepath.Join(quarantine, "code")
			if err := os.Symlink(target, held); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(quarantine, ".code.cloister-marker")
			// This is the exact truncated record which used to match when the
			// readlink producer contributed no bytes to the comparison.
			markerBytes := []byte("code\x00")
			if testCase.markerTarget != "" {
				markerBytes = []byte("code\x00" + filepath.Join(home, testCase.markerTarget) + "\x00")
			} else if testCase.name == "short successful readlink" {
				markerBytes = []byte("code\x00/short")
			}
			if err := os.WriteFile(marker, markerBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			backend := &localGuestBackend{captureRewrite: cleanupToolFaultRewrite(t, "readlink", 2, testCase.action)}
			profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

			report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
			if err != nil {
				t.Fatal(err)
			}
			want := "~/.cloister-alias-quarantine.Fail01/code"
			if !reflect.DeepEqual(report.UnverifiedQuarantineEntries, []string{want}) {
				t.Fatalf("unverified quarantine entries = %#v, want [%s]", report.UnverifiedQuarantineEntries, want)
			}
			if _, err := os.Lstat(filepath.Join(home, "code")); !os.IsNotExist(err) {
				t.Fatalf("unverified recovery moved an entry into ~/code: %v", err)
			}
			if got, err := os.Readlink(held); err != nil || got != target {
				t.Fatalf("unverified held symlink changed: target=%q err=%v", got, err)
			}
		})
	}
}

func TestPruneWorkspaceAliasesOrphanMarkerProducerFailureIsReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "personal-code")
	code := filepath.Join(home, "code")
	if err := os.Symlink(target, code); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(home, ".cloister-alias-quarantine.Orph01")
	if err := os.Mkdir(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(quarantine, ".code.cloister-marker")
	if err := os.WriteFile(marker, []byte("code\x00"+target+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &localGuestBackend{captureRewrite: cleanupToolFaultRewrite(t, "readlink", 2, `__CLOISTER_ACTUAL_TOOL__ "$@"; return 70`)}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.UnverifiedAliases, []string{"~/code"}) {
		t.Fatalf("unverified aliases = %#v, want [~/code]", report.UnverifiedAliases)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("producer failure removed orphan marker: %v", err)
	}
	if got, err := os.Readlink(code); err != nil || got != target {
		t.Fatalf("producer failure changed home alias: target=%q err=%v", got, err)
	}
}

func TestPruneWorkspaceAliasesRealpathFailuresNeverAuthorizeDeletion(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		call   int
		action string
	}{
		{name: "target status", call: 2, action: `__CLOISTER_ACTUAL_TOOL__ "$@"; return 70`},
		{name: "legacy status", call: 3, action: `__CLOISTER_ACTUAL_TOOL__ "$@"; return 70`},
		{name: "empty versus empty", call: -2, action: "return 0"},
		{name: "short target", call: 2, action: "printf '/short'; return 0"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			legacyRoot := filepath.Join(home, "host-workspace")
			code := filepath.Join(home, "code")
			if err := os.Symlink(legacyRoot, code); err != nil {
				t.Fatal(err)
			}
			backend := &localGuestBackend{captureRewrite: cleanupToolFaultRewrite(t, "realpath", testCase.call, testCase.action)}
			profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

			report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(report.UnverifiedAliases, []string{"~/code"}) {
				t.Fatalf("unverified aliases = %#v, want [~/code]", report.UnverifiedAliases)
			}
			if got, err := os.Readlink(code); err != nil || got != legacyRoot {
				t.Fatalf("realpath failure deleted legacy alias: target=%q err=%v", got, err)
			}
		})
	}
}

func TestPruneWorkspaceAliasesComparisonFailuresFailClosed(t *testing.T) {
	for _, testCase := range []struct {
		name string
		call int
	}{
		{name: "marker comparison", call: 3},
		{name: "resolved-target comparison", call: 4},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			legacyRoot := filepath.Join(home, "host-workspace")
			code := filepath.Join(home, "code")
			if err := os.Symlink(legacyRoot, code); err != nil {
				t.Fatal(err)
			}
			backend := &localGuestBackend{captureRewrite: cleanupToolFaultRewrite(t, "cmp", testCase.call, "return 2")}
			profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

			report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(report.UnverifiedAliases, []string{"~/code"}) {
				t.Fatalf("unverified aliases = %#v, want [~/code]", report.UnverifiedAliases)
			}
			if got, err := os.Readlink(code); err != nil || got != legacyRoot {
				t.Fatalf("comparison failure deleted legacy alias: target=%q err=%v", got, err)
			}
		})
	}
}

func TestPruneWorkspaceAliasesRecoveryComparisonFailureDoesNotRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	quarantine := filepath.Join(home, ".cloister-alias-quarantine.Cmp001")
	if err := os.Mkdir(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "personal-code")
	held := filepath.Join(quarantine, "code")
	if err := os.Symlink(target, held); err != nil {
		t.Fatal(err)
	}
	writeAliasQuarantineMarker(t, quarantine, "code", target)
	backend := &localGuestBackend{captureRewrite: cleanupToolFaultRewrite(t, "cmp", 3, "return 2")}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
	if err != nil {
		t.Fatal(err)
	}
	want := "~/.cloister-alias-quarantine.Cmp001/code"
	if !reflect.DeepEqual(report.UnverifiedQuarantineEntries, []string{want}) {
		t.Fatalf("unverified quarantine entries = %#v, want [%s]", report.UnverifiedQuarantineEntries, want)
	}
	if _, err := os.Lstat(filepath.Join(home, "code")); !os.IsNotExist(err) {
		t.Fatalf("failed comparison restored quarantined alias: %v", err)
	}
	if got, err := os.Readlink(held); err != nil || got != target {
		t.Fatalf("failed comparison changed held alias: target=%q err=%v", got, err)
	}
}

func TestPruneWorkspaceAliasesOrphanComparisonFailureIsReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "personal-code")
	code := filepath.Join(home, "code")
	if err := os.Symlink(target, code); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(home, ".cloister-alias-quarantine.Cmp002")
	if err := os.Mkdir(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	writeAliasQuarantineMarker(t, quarantine, "code", target)
	marker := filepath.Join(quarantine, ".code.cloister-marker")
	backend := &localGuestBackend{captureRewrite: cleanupToolFaultRewrite(t, "cmp", 3, "return 2")}
	profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, filepath.Join(home, "host-workspace"), filepath.Join(home, "workspaces"))

	report, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.UnverifiedAliases, []string{"~/code"}) {
		t.Fatalf("unverified aliases = %#v, want [~/code]", report.UnverifiedAliases)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("failed comparison removed orphan marker: %v", err)
	}
	if got, err := os.Readlink(code); err != nil || got != target {
		t.Fatalf("failed comparison changed home alias: target=%q err=%v", got, err)
	}
}

func TestPruneWorkspaceAliasesDependencyProbeFailsBeforeMovingAliases(t *testing.T) {
	for _, tool := range []string{"readlink", "realpath"} {
		t.Run(tool, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			legacyRoot := filepath.Join(home, "host-workspace")
			code := filepath.Join(home, "code")
			if err := os.Symlink(legacyRoot, code); err != nil {
				t.Fatal(err)
			}
			backend := &localGuestBackend{captureRewrite: cleanupToolFaultRewrite(t, tool, 1, "return 70")}
			profile := managedWorkspaceProfile(config.WorkspaceModeWorkspace, legacyRoot, filepath.Join(home, "workspaces"))

			_, err := (&Engine{}).PruneWorkspaceAliases("work", profile, backend)
			if err == nil || !strings.Contains(err.Error(), "safety check failed") || !strings.Contains(err.Error(), tool) {
				t.Fatalf("dependency probe error = %v, want clear %s safety failure", err, tool)
			}
			if got, err := os.Readlink(code); err != nil || got != legacyRoot {
				t.Fatalf("dependency probe moved alias: target=%q err=%v", got, err)
			}
		})
	}
}

// TestPruneWorkspaceAliasesGeneratesSafeScript checks the guest script both for
// shell validity and for the two properties that keep it from destroying
// anything: it must never target the synchronized copies under ~/workspaces,
// and it must quote the workspace root it was given.
func TestPruneWorkspaceAliasesGeneratesSafeScript(t *testing.T) {
	backend := &vm.MockBackend{}
	engine := &Engine{}

	if _, err := engine.PruneWorkspaceAliases("work", &config.Profile{
		StartDir:  "/Users/someone/Code/a collection",
		Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeWorkspace},
	}, backend); err != nil {
		t.Fatalf("PruneWorkspaceAliases() error = %v", err)
	}
	if len(backend.SSHScriptCalls) != 1 {
		t.Fatalf("guest scripts = %d, want 1", len(backend.SSHScriptCalls))
	}
	script := backend.SSHScriptCalls[0].Script

	if !strings.Contains(script, `'/Users/someone/Code/a collection'`) {
		t.Errorf("workspace root is not shell-quoted; got:\n%s", script)
	}
	if !strings.Contains(script, `"$HOME"/workspaces`) {
		t.Errorf("script does not exempt ~/workspaces; got:\n%s", script)
	}
	if !strings.Contains(script, `exec 9<"$HOME"`) || !strings.Contains(script, "flock -w 2 -E 75 9") {
		t.Errorf("script does not serialize quarantine recovery; got:\n%s", script)
	}
	for _, guard := range []string{
		`[ ! -L "$held" ]`,
		`[ -f "$marker" ] && [ ! -L "$marker" ] && [ -s "$marker" ]`,
		`printf '%s\0' "$name"`,
		`readlink -z -- "$link" > "$output" || return 1`,
		`read_single_nul_value "$output"`,
		`realpath -mz -- "$path" > "$output" || return 1`,
	} {
		if !strings.Contains(script, guard) {
			t.Errorf("script lacks recovery guard %q; got:\n%s", guard, script)
		}
	}
	markerWrite := strings.Index(script, `if ! write_marker "$name" "$alias" "$marker"; then`)
	if markerWrite < 0 || !strings.Contains(script[markerWrite:], `if ! mv -T -- "$alias" "$held"; then`) {
		t.Errorf("script does not persist the marker before moving an alias; got:\n%s", script)
	}
	for _, forbidden := range []string{"rm -rf", "rm -r ", `rmdir -- "$alias"`, "-delete"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("script uses %q; only verified symlinks may be removed:\n%s", forbidden, script)
		}
	}

	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available to syntax-check the generated script")
	}
	check := exec.Command(shell, "-n")
	check.Stdin = strings.NewReader(script)
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("generated script is not valid bash: %v\n%s\n--- script ---\n%s", err, out, script)
	}
}

// TestParseLeftoverReportNamesContainersUsingTheDirectory covers the case that
// makes the difference between a helpful report and one that talks a reader
// into deleting live data: a directory left over from a mounted workspace can
// be the bind-mounted storage of a running container rather than an abandoned
// copy.
func TestParseLeftoverReportNamesContainersUsingTheDirectory(t *testing.T) {
	for _, testCase := range []struct {
		name string
		out  string
		want string
	}{
		{
			name: "nothing left behind",
			out:  "\n",
			want: "",
		},
		{
			name: "leftover with nothing using it",
			out:  "517M left in /Users/someone/Code/collection\t\n",
			want: "517M left in /Users/someone/Code/collection",
		},
		{
			name: "leftover a running container depends on",
			out:  "517M left in /Users/someone/Code/collection\tgraph-db\n",
			want: "517M left in /Users/someone/Code/collection, in use by running container(s): graph-db",
		},
		{
			name: "leftover several containers depend on",
			out:  "517M left in /Users/someone/Code/collection\tgraph-db cache\n",
			want: "517M left in /Users/someone/Code/collection, in use by running container(s): graph-db cache",
		},
	} {
		if got := parseLeftoverReport(testCase.out); got != testCase.want {
			t.Errorf("%s: parseLeftoverReport(%q) = %q, want %q", testCase.name, testCase.out, got, testCase.want)
		}
	}
}

// TestEveryScriptSectionMarkerIsRecognized ties the progress sink to the
// banners the shipped scripts actually print. The sink reads those markers to
// name the sub-step running, so a script whose banner drifts out of the
// recognized shape would silently stop reporting progress rather than fail.
func TestEveryScriptSectionMarkerIsRecognized(t *testing.T) {
	t.Parallel()

	pattern := regexp.MustCompile(`^\s*echo "(=== .+ ===)"\s*$`)
	total := 0

	for _, name := range embeddedScripts {
		raw, err := Scripts.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			match := pattern.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			banner := match[1]
			// Shell would expand these before the marker reached the sink, so
			// the literal is not what a run prints.
			if strings.ContainsAny(banner, "$`") {
				continue
			}
			total++

			var seen []string
			sink := runlog.NewSink(io.Discard, func(s string) { seen = append(seen, s) }, 5)
			if _, err := sink.Write([]byte(banner + "\n")); err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(banner, "==="), "==="))
			if len(seen) != 1 || seen[0] != want {
				t.Errorf("%s: banner %q reported as %v, want [%q]", name, banner, seen, want)
			}
		}
	}

	// A count of zero would mean the pattern stopped matching the scripts and
	// the loop above asserted nothing at all.
	if total < 20 {
		t.Fatalf("found only %d section banners across the scripts; the marker convention likely changed", total)
	}
	t.Logf("verified %d section banners across %d scripts", total, len(embeddedScripts))
}
