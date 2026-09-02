package linux

import (
	"bytes"
	"net"
	"os/exec"
	"strings"
	"testing"
	"text/template"
	"time"

	"cloister.io/internal/config"
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
// projects arrive as synchronized copies neither creates nor keeps the
// ~/workspace and ~/code aliases, which on such a profile would point at an
// empty look-alike of the host tree rather than at any real project.
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
	if !strings.Contains(out, `rm -f "$stale_alias"`) {
		t.Errorf("managed-workspace bashrc does not remove stale aliases; got:\n%s", out)
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
	backend := &vm.MockBackend{}
	engine := &Engine{}

	leftover, err := engine.PruneWorkspaceAliases("dev", &config.Profile{
		StartDir:  "~/code",
		Workspace: config.WorkspaceConfig{Mode: config.WorkspaceModeVirtiofs},
	}, backend)
	if err != nil {
		t.Fatalf("PruneWorkspaceAliases() error = %v", err)
	}
	if leftover != "" {
		t.Errorf("leftover = %q, want empty", leftover)
	}
	if len(backend.SSHScriptCalls) != 0 {
		t.Errorf("mounted profile ran %d guest scripts, want 0", len(backend.SSHScriptCalls))
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
	for _, forbidden := range []string{"rm -rf", "rm -r "} {
		if strings.Contains(script, forbidden) {
			t.Errorf("script uses %q; only empty directories and symlinks may be removed:\n%s", forbidden, script)
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
