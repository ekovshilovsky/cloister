// Package linux implements the Provisioner interface for Linux guest VMs. All
// provisioning scripts and configuration templates are embedded at compile time
// so that the cloister binary is fully self-contained. Each public method
// accepts a vm.Backend to decouple the provisioning logic from any specific
// hypervisor.
package linux

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	"cloister.io/internal/config"
	profilepkg "cloister.io/internal/profile"
	"cloister.io/internal/provision/report"
	"cloister.io/internal/tunnel"
	"cloister.io/internal/vm"
	"cloister.io/internal/vmconfig"
)

//go:embed scripts/*
var Scripts embed.FS

//go:embed templates/*
var Templates embed.FS

// Engine implements provision.Provisioner for Linux guest VMs. It embeds all
// provisioning scripts and templates and executes them inside the VM via the
// supplied vm.Backend.
type Engine struct {
	// Out receives the guest output of the scripts this engine runs. A nil Out
	// means the terminal, which is what a caller that shows the output itself
	// wants; a caller reporting progress instead points this at a run log.
	//
	// It is read at the start of each operation rather than held, so a caller
	// running steps in sequence can repoint it between them. Nothing here runs
	// steps concurrently; a shared engine used that way would interleave two
	// steps' output into one destination.
	Out io.Writer

	// Steps is where Run reports its progress. A nil Steps reports nothing and
	// sends the guest output to Out, which is what a caller showing that output
	// itself already gets.
	Steps report.Reporter
}

// steps is the progress destination, defaulting to no reporting at all.
func (e *Engine) steps() report.Reporter {
	if e == nil || e.Steps == nil {
		return report.Discarded{Out: e.out()}
	}
	return e.Steps
}

// out is the destination for guest output, defaulting to the terminal.
func (e *Engine) out() io.Writer {
	if e == nil || e.Out == nil {
		return os.Stdout
	}
	return e.Out
}

// Run executes the full provisioning sequence for the given profile inside the
// corresponding VM. The sequence is:
//  1. Base tools (git, GitHub CLI, curl, NVM, pnpm, Claude Code)
//  2. Each requested toolchain stack in order
//  3. GPG key isolation (when GPGSigning is enabled)
//  4. Deployment of the managed ~/.bashrc
//  5. Git identity and signing configuration from host
//  6. GitHub CLI authentication from host
//  7. VM-side config file for the cloister-vm toolkit
//  8. Plugin configuration sync from host with path translation
//  9. Agent runtime setup (when Agent is configured)
//  10. Read-only re-mount enforcement for sensitive host-shared directories
//  11. Any custom per-profile provisioning hooks present on the host
func (e *Engine) Run(profile string, p *config.Profile, backend vm.Backend) error {
	steps := e.steps()

	// Step 1: Base provisioning installs the common toolset shared by all profiles.
	// CLOISTER_GPG_LOCAL toggles base.sh's gpg-agent policy: when set, the
	// local agent is unmasked so the user manages GPG inside the VM; when
	// unset, the local agent is masked (default; required by gpg_signing's
	// host-agent forwarding to bind the runtime socket path).
	baseStep := steps.Step("Base tools")
	baseErr := error(nil)
	if p.GpgLocal {
		baseErr = RunScriptWithEnvTo(profile, "scripts/base.sh", "CLOISTER_GPG_LOCAL=1", backend, baseStep.Writer())
	} else {
		baseErr = RunScriptTo(profile, "scripts/base.sh", backend, baseStep.Writer())
	}
	if baseErr != nil {
		baseStep.Fail()
		return fmt.Errorf("base provisioning: %w", baseErr)
	}
	baseStep.Done()

	// Step 2: Stack provisioning installs each requested toolchain stack.
	// expandStackDependencies pulls in implicit dependencies (e.g. web →
	// art) so users do not have to remember to list supporting tooling
	// alongside the primary stack they actually requested.
	for _, stack := range expandStackDependencies(p.Stacks) {
		stackStep := steps.Step(stack + " stack")
		scriptName := fmt.Sprintf("scripts/stack-%s.sh", stack)
		if err := RunScriptTo(profile, scriptName, backend, stackStep.Writer()); err != nil {
			stackStep.Fail()
			return fmt.Errorf("%s stack: %w", stack, err)
		}
		stackStep.Done()
	}

	// Post-provisioning host detection warnings for stack-specific services.
	for _, stack := range p.Stacks {
		if stack == "ollama" {
			printOllamaHostWarning()
		}
	}

	// Step 3: GPG signing imports the host public key into the VM keyring and
	// configures gpg.conf / sshd_config for forwarded-agent signing. The
	// reverse-forwarded gpg-agent socket itself is started by the tunnel
	// registry on every VM entry (see cmd/enter.go and internal/tunnel), so
	// provisioning does not start it here. Failures are non-fatal: the user
	// can fix host state and re-enter the profile to retry.
	if p.GPGSigning {
		gpgStep := steps.Step("GPG isolation")
		e.Out = gpgStep.Writer()
		if err := e.DeployGPGKeys(profile, backend); err != nil {
			gpgStep.Warn(fmt.Sprintf("GPG public-key import: %v", err))
		} else {
			gpgStep.Done()
		}
	}

	// Step 4: Write the managed bashrc so PATH, environment variables, and the
	// configured start directory are applied for every interactive session.
	shellStep := steps.Step("Shell configuration")
	bashrcResult, err := e.deployBashrcWithResult(profile, p, backend, shellStep.Writer())
	if err != nil {
		shellStep.Fail()
		return fmt.Errorf("deploying bashrc: %w", err)
	}
	if bashrcResult.ReplacedSymlink {
		shellStep.Warn("replaced symbolic-link ~/.bashrc; its target was left unchanged")
	} else {
		shellStep.Done()
	}

	// Step 5: Deploy git identity and signing configuration from the host so
	// commits inside the VM use the same author and GPG signing settings.
	gitStep := steps.Step("Git configuration")
	e.Out = gitStep.Writer()
	if err := e.DeployGitConfig(profile, p, backend); err != nil {
		gitStep.Warn(fmt.Sprintf("git config: %v", err))
	} else {
		gitStep.Done()
	}

	// Step 6: Transfer GitHub CLI authentication from the host so that git
	// credential helpers and gh commands work inside the VM.
	ghStep := steps.Step("GitHub CLI authentication")
	if err := DeployGHAuthTo(profile, backend, ghStep.Writer()); err != nil {
		ghStep.Warn(fmt.Sprintf("gh auth: %v", err))
	} else {
		ghStep.Done()
	}

	// Step 7: Deploy VM-side config for the cloister-vm toolkit.
	vmConfigStep := steps.Step("VM toolkit configuration")
	e.Out = vmConfigStep.Writer()
	if err := e.DeployVMConfig(profile, p, backend, tunnel.BuiltinTunnelDefs(), bashrcData(profile, p).StartDir); err != nil {
		vmConfigStep.Warn(fmt.Sprintf("deploying VM config: %v", err))
	} else {
		vmConfigStep.Done()
	}

	// Step 8: Synchronize plugin index files and settings from the host into
	// the VM with translated paths so Claude Code plugins work correctly.
	pluginStep := steps.Step("Plugin configuration")
	hostHome, err := os.UserHomeDir()
	if err != nil {
		pluginStep.Warn(fmt.Sprintf("could not determine host home directory: %v", err))
	} else if err := SyncPlugins(profile, hostHome, backend, pluginStep.Writer()); err != nil {
		pluginStep.Warn(fmt.Sprintf("plugin sync: %v", err))
	} else {
		pluginStep.Done()
	}

	// Step 9: Agent setup — pull Docker image and install cleanup cron.
	if p.Agent != nil {
		agentStep := steps.Step("Agent runtime")
		if err := RunScriptWithEnvTo(profile, "scripts/agent-setup.sh",
			fmt.Sprintf("AGENT_IMAGE=%s", p.Agent.Image), backend, agentStep.Writer()); err != nil {
			agentStep.Fail()
			return fmt.Errorf("agent setup: %w", err)
		}
		agentStep.Done()
	}

	// Step 10: Re-enforce read-only mounts for sensitive directories. This is
	// best-effort: a failure is reported but does not abort provisioning.
	// For headless profiles, the script also locks down Claude extension
	// directories to prevent lateral movement attacks.
	mountStep := steps.Step("Read-only mounts")
	mountErr := error(nil)
	if p.Headless {
		mountErr = RunScriptWithEnvTo(profile, "scripts/read-only-mounts.sh", "CLOISTER_HEADLESS=1", backend, mountStep.Writer())
	} else {
		mountErr = RunScriptTo(profile, "scripts/read-only-mounts.sh", backend, mountStep.Writer())
	}
	if mountErr != nil {
		mountStep.Warn(fmt.Sprintf("read-only mount enforcement: %v", mountErr))
	} else {
		mountStep.Done()
	}

	// Step 11: Run the global hook first, then this profile's hook, when present.
	if err := e.runCustomHooks(profile, backend, steps); err != nil {
		return err
	}

	return nil
}

// DeployConfig re-deploys the managed bashrc and VM config into a running VM
// so that configuration changes take effect without a full rebuild.
func (e *Engine) DeployConfig(profile string, p *config.Profile, backend vm.Backend) error {
	if err := e.DeployBashrc(profile, p, backend); err != nil {
		return err
	}
	return e.DeployVMConfig(profile, p, backend, tunnel.BuiltinTunnelDefs(), bashrcData(profile, p).StartDir)
}

// DeployBashrc re-renders and deploys the managed bashrc into a running VM.
// This allows configuration changes (e.g., toggling claude_local) to take
// effect without a full rebuild.
func (e *Engine) DeployBashrc(profile string, p *config.Profile, backend vm.Backend) error {
	result, err := e.deployBashrcWithResult(profile, p, backend, e.out())
	if result.ReplacedSymlink {
		writeBashrcSymlinkNotice(e.out())
	}
	return err
}

func (e *Engine) deployBashrcWithResult(profile string, p *config.Profile, backend vm.Backend, out io.Writer) (guestWriteResult, error) {
	return deployTemplateWithResult(profile, "templates/bashrc.tmpl", "~/.bashrc", bashrcData(profile, p), backend, out)
}

const bashrcDigestPrefix = "cloister-bashrc-sha256:"

// BashrcReconcileResult reports how EnsureBashrc changed the managed file.
type BashrcReconcileResult struct {
	Changed         bool
	ReplacedSymlink bool
}

// EnsureBashrc compares the deployed managed bashrc with the content rendered
// from the current profile and embedded template. A differing or missing file
// is replaced; an exact regular-file match is left untouched. The result tells
// interactive callers whether local content or a symbolic link was replaced.
func (e *Engine) EnsureBashrc(profile string, p *config.Profile, backend vm.Backend) (BashrcReconcileResult, error) {
	rendered, err := renderTemplate("templates/bashrc.tmpl", bashrcData(profile, p))
	if err != nil {
		return BashrcReconcileResult{}, fmt.Errorf("rendering bashrc: %w", err)
	}
	// deployTemplate places one newline between the rendered content and its
	// heredoc delimiter. Include that byte so a file it deployed compares equal.
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(rendered+"\n")))
	script := `set -u
kind=regular
[ -L "$HOME/.bashrc" ] && kind=symlink
if [ ! -e "$HOME/.bashrc" ] && [ ! -L "$HOME/.bashrc" ]; then
	state=missing
elif digest=$(sha256sum -- "$HOME/.bashrc" 2>/dev/null); then
	state=${digest%% *}
else
	state=unreadable
fi
printf '` + bashrcDigestPrefix + `%s:%s\n' "$kind" "$state"
`
	out, err := backend.SSHCapture(profile, script)
	if err != nil {
		// An SSH/backend failure is categorically different from a guest-side
		// unreadable marker. Never overwrite when the guest did not complete
		// the comparison script and report its state successfully.
		return BashrcReconcileResult{}, fmt.Errorf("checking deployed bashrc: %w", err)
	}
	state := deployedBashrcState(out)
	if state.Kind == "regular" && state.Digest == want {
		return BashrcReconcileResult{}, nil
	}
	deployResult, err := e.deployBashrcWithResult(profile, p, backend, e.out())
	if err != nil {
		return BashrcReconcileResult{}, fmt.Errorf("deploying current bashrc: %w", err)
	}
	return BashrcReconcileResult{Changed: true, ReplacedSymlink: deployResult.ReplacedSymlink}, nil
}

type bashrcState struct {
	Kind   string
	Digest string
}

// deployedBashrcState extracts the fixed-format state emitted after login
// initialization. Other output can precede it when a guest's shell profile is
// noisy. No marker is treated as a mismatch, which refreshes this managed file
// only after the guest command itself has completed successfully.
func deployedBashrcState(out string) bashrcState {
	for _, line := range strings.Split(out, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), bashrcDigestPrefix); ok {
			kind, digest, found := strings.Cut(strings.TrimSpace(value), ":")
			if found {
				return bashrcState{Kind: kind, Digest: digest}
			}
		}
	}
	return bashrcState{}
}

func writeBashrcSymlinkNotice(out io.Writer) {
	if out != nil {
		_, _ = fmt.Fprintln(out, "notice: ~/.bashrc was a symbolic link; Cloister replaced the link with a managed regular file and left its target unchanged")
	}
}

// DeployVMConfig writes the cloister-vm config file into the VM so the
// in-VM toolkit can read tunnel definitions, profile name, and workspace path.
func (e *Engine) DeployVMConfig(profile string, p *config.Profile, backend vm.Backend, tunnelDefs []vmconfig.TunnelDef, workspaceDir string) error {
	hostHome, _ := os.UserHomeDir()
	cfg := vmconfig.Config{
		Profile:     profile,
		Tunnels:     tunnelDefs,
		Workspace:   workspaceDir,
		HostHome:    hostHome,
		ClaudeLocal: p.ClaudeLocal,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling VM config: %w", err)
	}
	script := "mkdir -p ~/.cloister-vm\n" + atomicGuestWriteScript("~/.cloister-vm/config.json", string(data), false)
	_, err = backend.SSHScriptTo(profile, script, e.out())
	return err
}

// RunScriptTo reads the named embedded script and executes it inside the VM via
// a non-interactive SSH session on the supplied backend, sending the guest
// output to out so the caller can record it and report progress in its place.
// Exported for use by the commands that run individual scripts independently.
func RunScriptTo(profile, scriptPath string, backend vm.Backend, out io.Writer) error {
	data, err := Scripts.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", scriptPath, err)
	}
	_, err = backend.SSHScriptTo(profile, string(data), out)
	return err
}

// assembleScriptWithEnv reads an embedded script and prepends an environment
// variable export line. This is used to pass configuration flags to provisioning
// scripts that cannot accept command-line arguments.
func assembleScriptWithEnv(scriptPath, envLine string) (string, error) {
	data, err := Scripts.ReadFile(scriptPath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", scriptPath, err)
	}
	return fmt.Sprintf("export %s\n%s", envLine, string(data)), nil
}

// RunScriptWithEnvTo reads the named embedded script and executes it inside the
// VM with the specified environment variable exported before the script runs,
// sending the guest output to a caller-chosen destination.
func RunScriptWithEnvTo(profile, scriptPath, envLine string, backend vm.Backend, out io.Writer) error {
	script, err := assembleScriptWithEnv(scriptPath, envLine)
	if err != nil {
		return err
	}
	_, err = backend.SSHScriptTo(profile, script, out)
	return err
}

// deployTemplate renders the named embedded Go template and atomically replaces
// destPath from a fully written sibling temporary file, sending what the guest
// said to out.
//
// A template deploy is a guest command like any other, and a failed one is
// exactly when its output is worth having: without this, a step that could not
// write its file reports that it failed and nothing about why.
func deployTemplate(profile, tmplPath, destPath string, data interface{}, backend vm.Backend, out io.Writer) error {
	_, err := deployTemplateWithResult(profile, tmplPath, destPath, data, backend, out)
	return err
}

type guestWriteResult struct {
	ReplacedSymlink bool
}

const replacedSymlinkMarker = "cloister-atomic-write-replaced-symlink"

func deployTemplateWithResult(profile, tmplPath, destPath string, data interface{}, backend vm.Backend, out io.Writer) (guestWriteResult, error) {
	rendered, err := renderTemplate(tmplPath, data)
	if err != nil {
		return guestWriteResult{}, err
	}
	escaped := atomicGuestWriteScript(destPath, rendered, true)
	guest, sshErr := backend.SSHCommand(profile, escaped)
	cleaned, replacedSymlink := consumeGuestWriteMarker(guest)
	// Written whether or not the command succeeded, and before the error is
	// returned, so the failure tail has the guest's account of it.
	if out != nil {
		_, _ = io.WriteString(out, cleaned)
	}
	return guestWriteResult{ReplacedSymlink: replacedSymlink && sshErr == nil}, sshErr
}

// atomicGuestWriteScript writes a sibling temporary file completely before
// replacing destPath with rename(2). The destination is never opened for
// writing, so leaf symlinks and hardlinks cannot redirect or share the write,
// and a partial temp-file write leaves the old destination unchanged.
func atomicGuestWriteScript(destPath, content string, reportSymlink bool) string {
	dest := "dest=" + shellSingleQuote(destPath)
	if relative, ok := strings.CutPrefix(destPath, "~/"); ok {
		dest = `dest="$HOME/"` + shellSingleQuote(relative)
	}
	report := ""
	if reportSymlink {
		report = `
if [ "$replaced_symlink" -eq 1 ]; then
	printf '` + replacedSymlinkMarker + `\n'
fi`
	}
	return `set -eu
umask 077
` + dest + `
parent=${dest%/*}
base=${dest##*/}
tmp=$(mktemp -- "$parent/.${base}.cloister-tmp.XXXXXX")
cleanup_tmp() {
	[ -z "${tmp:-}" ] || rm -f -- "$tmp"
}
trap cleanup_tmp EXIT HUP INT TERM
cat > "$tmp" << 'CLOISTER_EOF'
` + content + `
CLOISTER_EOF
chmod 0600 "$tmp"
replaced_symlink=0
[ ! -L "$dest" ] || replaced_symlink=1
mv -fT -- "$tmp" "$dest"
tmp=
trap - EXIT HUP INT TERM` + report + `
`
}

func consumeGuestWriteMarker(out string) (string, bool) {
	var kept []string
	replaced := false
	for _, line := range strings.SplitAfter(out, "\n") {
		if strings.TrimSpace(line) == replacedSymlinkMarker {
			replaced = true
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, ""), replaced
}

// renderTemplate executes one embedded template without adding the newline
// that separates its output from deployTemplate's heredoc delimiter.
func renderTemplate(tmplPath string, data interface{}) (string, error) {
	tmplData, err := Templates.ReadFile(tmplPath)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New("").Parse(string(tmplData))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// buildDeployGPGKeysScript renders the bash script that runs inside the VM to
// configure GPG signing via host-side agent forwarding. It receives the host
// public-key armor and the full key fingerprint and returns a self-contained
// script that:
//
//   - ensures $HOME/.gnupg exists with mode 0700
//   - imports the public key and sets ultimate ownertrust
//   - writes gpg.conf with no-autostart so the gpg client never spawns a
//     local agent if the forwarded socket is unavailable
//   - drops a /etc/ssh/sshd_config.d/cloister-gpg.conf with
//     StreamLocalBindUnlink yes and reloads sshd
//
// Private key material is never referenced. The keyring lives at the default
// $HOME/.gnupg/, not the legacy $HOME/.gnupg-local/.
//
// Step ordering is load-bearing: the public-key import and ownertrust steps
// run BEFORE gpg.conf is written. Once gpg.conf contains "no-autostart", a
// gpg client invocation refuses to spawn a transient agent, which causes
// "gpg --batch --import" to fail on a fresh VM where no agent is running.
// We therefore remove any stale gpg.conf at the start of the script, perform
// the import, then write the desired gpg.conf last.
func buildDeployGPGKeysScript(pubKeyArmor, fingerprint string) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n")
	b.WriteString("mkdir -p \"$HOME/.gnupg\"\n")
	b.WriteString("chmod 700 \"$HOME/.gnupg\"\n")

	// Remove any pre-existing gpg.conf so a stale "no-autostart" directive
	// from a prior provisioning run does not block this run's gpg --import.
	// The desired gpg.conf is rewritten at the end of this script.
	//
	// Note: disabling the local gpg-agent (so the cloister-managed reverse
	// tunnel can claim /run/user/<uid>/gnupg/S.gpg-agent) happens earlier in
	// base.sh, on every cloister VM. By the time this GPG-signing path runs,
	// the local agent is already masked and could never have been started.
	b.WriteString("rm -f \"$HOME/.gnupg/gpg.conf\"\n")

	b.WriteString("cat << 'PUBKEY_EOF' | gpg --batch --import\n")
	b.WriteString(pubKeyArmor)
	if !strings.HasSuffix(pubKeyArmor, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("PUBKEY_EOF\n")

	if fingerprint != "" {
		b.WriteString(fmt.Sprintf("echo '%s:6:' | gpg --import-ownertrust\n", fingerprint))
	}

	b.WriteString("cat > \"$HOME/.gnupg/gpg.conf\" << 'GPG_CONF_EOF'\n")
	b.WriteString("# Managed by cloister: do not let gpg start a local agent.\n")
	b.WriteString("# The agent socket is reverse-tunneled from the macOS host.\n")
	b.WriteString("no-autostart\n")
	b.WriteString("GPG_CONF_EOF\n")
	b.WriteString("chmod 600 \"$HOME/.gnupg/gpg.conf\"\n")

	b.WriteString("sudo tee /etc/ssh/sshd_config.d/cloister-gpg.conf > /dev/null << 'SSHD_EOF'\n")
	b.WriteString("# Managed by cloister: required for reverse-forwarded gpg-agent socket\n")
	b.WriteString("# to rebind cleanly across SSH sessions.\n")
	b.WriteString("StreamLocalBindUnlink yes\n")
	b.WriteString("SSHD_EOF\n")
	b.WriteString("sudo systemctl reload ssh 2>/dev/null || sudo systemctl reload sshd 2>/dev/null || true\n")

	return b.String()
}

// buildDeployGPGKeysScriptForTest is the package-internal entry point used by
// engine_test.go to render a script with deterministic fixture inputs so the
// regression assertion does not depend on the host's gpg keyring.
func buildDeployGPGKeysScriptForTest() string {
	pubKey := "-----BEGIN PGP PUBLIC KEY BLOCK-----\n[fixture]\n-----END PGP PUBLIC KEY BLOCK-----"
	return buildDeployGPGKeysScript(pubKey, "1111222233334444555566667777888899990000")
}

// DeployGPGKeys imports the host's public GPG signing key into the VM's
// default keyring and configures the VM so that signing operations transit
// the host gpg-agent via cloister's reverse-forwarded extra-socket. No
// private key material is shipped.
func (e *Engine) DeployGPGKeys(profile string, backend vm.Backend) error {
	keyIDOut, err := exec.Command("git", "config", "--global", "user.signingkey").Output()
	if err != nil {
		return fmt.Errorf("no signing key configured in host git config")
	}
	keyID := strings.TrimSpace(string(keyIDOut))
	if keyID == "" {
		return fmt.Errorf("host git config user.signingkey is empty")
	}

	pubKey, err := exec.Command("gpg", "--armor", "--export", keyID).Output()
	if err != nil || len(pubKey) == 0 {
		return fmt.Errorf("exporting public key %s: %w", keyID, err)
	}

	fingerprint := ""
	if fpOut, err := exec.Command("gpg", "--with-colons", "--fingerprint", keyID).Output(); err == nil {
		for _, line := range strings.Split(string(fpOut), "\n") {
			if strings.HasPrefix(line, "fpr:") {
				parts := strings.Split(line, ":")
				if len(parts) >= 10 {
					fingerprint = parts[9]
				}
				break
			}
		}
	}

	script := buildDeployGPGKeysScript(string(pubKey), fingerprint)
	if _, err := backend.SSHScriptTo(profile, script, e.out()); err != nil {
		return fmt.Errorf("deploying GPG public key and config: %w", err)
	}
	return nil
}

// gitconfigTemplateData holds the values substituted into templates/gitconfig.tmpl.
type gitconfigTemplateData struct {
	GitName    string
	GitEmail   string
	GPGSigning bool
	GPGKeyID   string
}

// readHostGitConfig reads the host's global git configuration values needed
// for the gitconfig template. Returns zero values for any fields that cannot
// be read (git not configured on host).
func readHostGitConfig() gitconfigTemplateData {
	data := gitconfigTemplateData{}
	if out, err := exec.Command("git", "config", "--global", "user.name").Output(); err == nil {
		data.GitName = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "config", "--global", "user.email").Output(); err == nil {
		data.GitEmail = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "config", "--global", "user.signingkey").Output(); err == nil {
		data.GPGKeyID = strings.TrimSpace(string(out))
	}
	return data
}

// DeployGitConfig reads the host's git identity and signing configuration,
// renders the gitconfig template, and deploys it as ~/.gitconfig in the VM.
func (e *Engine) DeployGitConfig(profile string, p *config.Profile, backend vm.Backend) error {
	data := readHostGitConfig()
	if data.GitName == "" || data.GitEmail == "" {
		return fmt.Errorf("host git config missing user.name or user.email")
	}
	data.GPGSigning = p.GPGSigning
	return deployTemplate(profile, "templates/gitconfig.tmpl", "~/.gitconfig", data, backend, e.out())
}

// DeployGHAuthTo transfers the host's GitHub CLI authentication into the VM
// so that git credential helpers and gh CLI commands work without manual login,
// sending the guest output to a caller-chosen destination. Requires gh to be
// installed on the host and authenticated.
func DeployGHAuthTo(profile string, backend vm.Backend, out io.Writer) error {
	token, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return fmt.Errorf("reading host gh token: %w (is gh authenticated?)", err)
	}
	tokenStr := strings.TrimSpace(string(token))
	if tokenStr == "" {
		return fmt.Errorf("host gh token is empty")
	}
	script := fmt.Sprintf("echo '%s' | gh auth login --with-token 2>/dev/null", tokenStr)
	_, err = backend.SSHScriptTo(profile, script, out)
	return err
}

// bashrcTemplateData holds the values substituted into templates/bashrc.tmpl.
type bashrcTemplateData struct {
	// Profile is the cloister profile name, rendered as a comment header so
	// it is easy to identify which VM a given bashrc belongs to.
	Profile string

	// StartDir is the directory the shell changes into at login. Falls back to
	// ~/code when the profile does not specify a start directory.
	StartDir string

	// GPGSigning indicates whether this profile signs commits via the host
	// gpg-agent. Provisioning starts a reverse-forwarded extra-socket tunnel
	// when true. The bashrc template no longer consumes this field directly:
	// the forwarded socket is bound at the standard $HOME/.gnupg/S.gpg-agent
	// path, so no GNUPGHOME redirection is needed.
	GPGSigning bool

	// ClaudeLocal enables offline Claude Code by pointing it at the host's
	// Ollama server via the Anthropic Messages API compatibility layer.
	ClaudeLocal bool

	// ManagedWorkspace marks a profile whose projects reach the guest as
	// synchronized copies under ~/workspaces. Such a profile has no host
	// workspace mount, so the template neither creates the ~/workspace and
	// ~/code mount aliases nor treats StartDir as a guest path.
	ManagedWorkspace bool
}

// ResolveStartDir returns the given startDir or the default "~/code" when
// empty. This is the canonical fallback used by both the bashrc template and
// the VM config deployment.
func ResolveStartDir(startDir string) string {
	if startDir == "" {
		return "~/code"
	}
	return startDir
}

// bashrcData constructs the template data for the bashrc template from the
// given profile name and its configuration.
func bashrcData(profile string, p *config.Profile) bashrcTemplateData {
	return bashrcTemplateData{
		Profile:          profile,
		StartDir:         ResolveStartDir(p.StartDir),
		GPGSigning:       p.GPGSigning,
		ClaudeLocal:      p.ClaudeLocal,
		ManagedWorkspace: p.UsesManagedWorkspace(),
	}
}

// maxProvisionHookSize bounds the host data copied into a guest provisioning
// command. Provisioning hooks are shell scripts, so one MiB leaves ample room
// for real scripts without allowing an accidental or hostile file to exhaust
// memory during provisioning.
const maxProvisionHookSize int64 = 1 << 20

// readCustomHook reads one hook without following a symbolic link. The first
// Lstat distinguishes an absent optional hook from a configured path that is
// unusable; the no-following, non-blocking open and second Stat close the race
// where the path changes to a symlink or FIFO between inspection and opening.
func readCustomHook(path string) ([]byte, bool, error) {
	pathInfo, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, true, fmt.Errorf("must be a regular file, not a symbolic link")
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, true, fmt.Errorf("must be a regular file (mode %s)", pathInfo.Mode())
	}
	if pathInfo.Size() > maxProvisionHookSize {
		return nil, true, fmt.Errorf("is %d bytes; maximum size is %d bytes", pathInfo.Size(), maxProvisionHookSize)
	}

	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, true, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()

	openInfo, err := file.Stat()
	if err != nil {
		return nil, true, err
	}
	if !openInfo.Mode().IsRegular() {
		return nil, true, fmt.Errorf("must be a regular file (mode %s)", openInfo.Mode())
	}
	if !os.SameFile(pathInfo, openInfo) {
		return nil, true, fmt.Errorf("changed while it was being opened")
	}
	if openInfo.Size() > maxProvisionHookSize {
		return nil, true, fmt.Errorf("is %d bytes; maximum size is %d bytes", openInfo.Size(), maxProvisionHookSize)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxProvisionHookSize+1))
	if err != nil {
		return nil, true, err
	}
	if int64(len(data)) > maxProvisionHookSize {
		return nil, true, fmt.Errorf("grew beyond the maximum size of %d bytes while being read", maxProvisionHookSize)
	}
	return data, true, nil
}

// runCustomHooks executes the two hook names defined by the provisioning
// contract. Regular host files at those exact paths are piped to the guest in
// global-then-profile order; an unsafe, unreadable, or failed hook stops
// provisioning so it cannot be silently skipped or reported as run.
func (e *Engine) runCustomHooks(profile string, backend vm.Backend, steps report.Reporter) error {
	if err := profilepkg.ValidateName(profile); err != nil {
		return fmt.Errorf("validating profile for provisioning hooks: %w", err)
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return fmt.Errorf("resolving provisioning hook directory: %w", err)
	}
	hooks := []struct {
		path string
		step string
		err  string
	}{
		{filepath.Join(dir, "provision.sh"), "Global provisioning hook", "global provisioning hook"},
		{filepath.Join(dir, "provision-"+profile+".sh"), profile + " provisioning hook", "profile provisioning hook"},
	}
	for _, hook := range hooks {
		data, exists, readErr := readCustomHook(hook.path)
		if !exists {
			continue
		}
		step := steps.Step(hook.step)
		if readErr != nil {
			step.Fail()
			return fmt.Errorf("reading %s: %w", hook.err, readErr)
		}
		if _, runErr := backend.SSHScriptTo(profile, string(data), step.Writer()); runErr != nil {
			step.Fail()
			return fmt.Errorf("running %s: %w", hook.err, runErr)
		}
		step.Done()
	}
	return nil
}

// checkHost dials host:port over TCP with the given timeout and returns true
// when the connection is accepted. It is used to probe local services before
// printing advisory messages to the user.
func checkHost(host string, port int, timeout time.Duration) bool {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// printOllamaHostWarning checks whether the host Ollama server is running and
// prints guidance when it is not detected.
func printOllamaHostWarning() {
	if !checkHost("127.0.0.1", 11434, 500*time.Millisecond) {
		fmt.Println("  ⚠ Host Ollama not detected on port 11434.")
		fmt.Println("    Install on your Mac for GPU-accelerated inference: brew install ollama")
		fmt.Println("    The ollama CLI is installed in the VM but has no server to connect to")
		fmt.Println("    until host Ollama is running and the tunnel is forwarded.")
	} else {
		fmt.Println("  ✓ Host Ollama detected — will be tunneled into VM on entry")
	}
}

// shellSingleQuote renders a value as one safely quoted POSIX shell word.
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// WorkspaceCleanupReport describes guest-local entries that could not be
// removed automatically while preparing a managed workspace layout.
type WorkspaceCleanupReport struct {
	// PreservedAliases names the legacy alias paths occupied by non-symlinks.
	// They are user data regardless of how they came to exist.
	PreservedAliases []string

	// StrandedAliases names entries left in a cleanup quarantine because their
	// original guest-home path was occupied. They remain untouched for manual
	// recovery.
	StrandedAliases []string

	// UnverifiedQuarantineEntries names objects that resemble interrupted
	// cleanup state but lack a matching Cloister marker. They are never moved
	// or deleted automatically.
	UnverifiedQuarantineEntries []string

	// Leftover describes a non-empty guest-local copy of the former workspace
	// mount path. It may still be live container storage.
	Leftover string
}

// HasWarnings reports whether preparing the layout found anything that needs
// the user's attention.
func (r WorkspaceCleanupReport) HasWarnings() bool {
	return len(r.PreservedAliases) > 0 || len(r.StrandedAliases) > 0 || len(r.UnverifiedQuarantineEntries) > 0 || r.Leftover != ""
}

const (
	workspaceCleanupLockWaitSeconds = 2
	workspaceCleanupLockTimeoutMark = "cloister-cleanup-lock-timeout"
)

// PruneWorkspaceAliases cleans up the guest-side remnants of a mounted
// workspace on a profile that has since moved to synchronized copies.
//
// Two remnants outlive the switch. The ~/workspace and ~/code aliases are
// recreated by older bashrc revisions on every login and point at the host
// workspace path, which on such a profile is not a mount but an ordinary guest
// directory holding, at most, empty stubs. That directory is the second
// remnant. Both make the guest look as though it carries the host tree.
//
// An alias is removed only when it is still a symlink and its resolved target
// equals the start-directory mount path used by the legacy bashrc. A user-made
// symlink to another target is therefore left alone. Any non-symlink at an
// alias path is reported instead of deleted, because it is user data regardless
// of how it got there.
//
// Before inspecting aliases, cleanup recovers entries left in private
// quarantine directories by an interrupted earlier run. Entry and repair share
// this path and serialize recovery so neither can treat the other's active
// quarantine as abandoned.
//
// The former mount path is inspected but never deleted. When it holds real
// content the report names any running containers using it so the reader does
// not mistake live service storage for an abandoned copy.
func (e *Engine) PruneWorkspaceAliases(profile string, p *config.Profile, backend vm.Backend) (WorkspaceCleanupReport, error) {
	if !p.UsesManagedWorkspace() {
		return WorkspaceCleanupReport{}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return WorkspaceCleanupReport{}, fmt.Errorf("determining host home: %w", err)
	}
	legacyRoot, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return WorkspaceCleanupReport{}, fmt.Errorf("resolving legacy workspace mount: %w", err)
	}
	root, err := config.ResolveWorkspaceDir(workspaceRootValue(p), home)
	if err != nil {
		return WorkspaceCleanupReport{}, fmt.Errorf("resolving workspace root: %w", err)
	}

	script := `set -eu
umask 077

# Entry and repair can run concurrently. Serialize them so recovery never
# mistakes another live cleanup's quarantine for one abandoned by a crash.
# Lock the home directory itself so acquiring the lock creates or truncates no
# user-controlled pathname. Cloister's Ubuntu guests use util-linux flock;
# -E 75 reserves status 75 specifically for lock contention. Re-check these
# option and exit-status semantics before changing the guest base system.
exec 9<"$HOME"
if flock -w ` + fmt.Sprintf("%d", workspaceCleanupLockWaitSeconds) + ` -E 75 9; then
	:
else
	lock_status=$?
	if [ "$lock_status" -eq 75 ]; then
		printf '` + workspaceCleanupLockTimeoutMark + `\n'
	fi
	exit "$lock_status"
fi

# A SIGKILL cannot run shell traps. Recover quarantines abandoned by an older
# cleanup before moving any new aliases. A matching directory name only makes
# an entry a recovery candidate; recovery requires its type and metadata to
# match the state this code writes.
for prior in "$HOME"/.cloister-alias-quarantine.*; do
	[ -d "$prior" ] || continue
	[ ! -L "$prior" ] || continue
	prior_name=${prior##*/}
	suffix=${prior_name#.cloister-alias-quarantine.}
	[ ${#suffix} -eq 6 ] || continue
	case "$suffix" in
		*[!A-Za-z0-9]*) continue ;;
	esac
	for name in workspace code; do
		held="$prior/$name"
		marker="$prior/.${name}.cloister-marker"
		if [ ! -e "$held" ] && [ ! -L "$held" ]; then
			# Marker-first creation can be interrupted before the alias moves.
			# Remove that orphan marker only when it exactly describes the
			# symlink that is still safely present at the original path.
			if [ -f "$marker" ] && [ ! -L "$marker" ] && [ -L "$HOME/$name" ] && {
				printf '%s\0' "$name"
				readlink -z -- "$HOME/$name"
			} | cmp -s -- "$marker" -; then
				rm -- "$marker"
			fi
			continue
		fi
		# Cloister quarantines only symlinks. The marker is the original alias
		# name and readlink target, each NUL-terminated so neither Bash command
		# substitution nor line parsing can normalize their bytes.
		# Refuse regular files, directories, missing markers, marker symlinks,
		# and targets that do not match byte-for-byte.
		if [ ! -L "$held" ] || [ ! -f "$marker" ] || [ -L "$marker" ] || ! {
			printf '%s\0' "$name"
			readlink -z -- "$held"
		} | cmp -s -- "$marker" -; then
			printf 'cloister-unverified-quarantine:%s/%s\n' "$prior_name" "$name"
			continue
		fi
		# GNU mv -n does not overwrite an occupied path. Checking the source
		# afterward also covers a destination created concurrently by an
		# unrelated same-UID process.
		mv -nT -- "$held" "$HOME/$name"
		if [ -e "$held" ] || [ -L "$held" ]; then
			printf 'cloister-stranded-alias:%s/%s\n' "$prior_name" "$name"
		else
			rm -- "$marker"
		fi
	done
	# Unknown or stranded entries keep the private directory in place.
	rmdir -- "$prior" 2>/dev/null || true
done

legacy=` + shellSingleQuote(legacyRoot) + `
quarantine=$(mktemp -d -- "$HOME/.cloister-alias-quarantine.XXXXXX")
cleanup_quarantine() {
	for name in workspace code; do
		held="$quarantine/$name"
		marker="$quarantine/.${name}.cloister-marker"
		# Marker-first creation means an interrupt before rename leaves the
		# original alias in place. Signals that run this trap may remove only
		# such an orphaned regular marker; a marker beside a held entry is the
		# recovery record and must remain.
		if [ ! -e "$held" ] && [ ! -L "$held" ] && [ -f "$marker" ] && [ ! -L "$marker" ]; then
			rm -- "$marker"
		fi
	done
	rmdir -- "$quarantine" 2>/dev/null || true
}
trap cleanup_quarantine EXIT HUP INT TERM
for alias in "$HOME/workspace" "$HOME/code"; do
    if [ -L "$alias" ]; then
		name=${alias##*/}
		held="$quarantine/$name"
		marker="$quarantine/.${name}.cloister-marker"
		# Write and close the marker before moving the alias. A crash before the
		# rename leaves the alias safely in $HOME; a crash after it leaves the
		# marker and symlink together for validated recovery.
		if ! {
			printf '%s\0' "$name"
			readlink -z -- "$alias"
		} > "$marker"; then
			rm -f -- "$marker"
			[ ! -e "$alias" ] || printf 'cloister-preserved-alias:%s\n' "$name"
			continue
		fi
		chmod 0600 "$marker"
		# Moving the directory entry first closes the check/remove race. Every
		# later check and unlink operates on the same quarantined object.
		if ! mv -T -- "$alias" "$held"; then
			rm -- "$marker"
			[ ! -e "$alias" ] || printf 'cloister-preserved-alias:%s\n' "${alias##*/}"
			continue
		fi
		preserved=0
		if [ ! -L "$held" ] || ! {
			printf '%s\0' "$name"
			readlink -z -- "$held"
		} | cmp -s -- "$marker" -; then
			preserved=1
		fi
		if [ "$preserved" -eq 0 ]; then
			# Bash variables can contain newlines but command substitution removes
			# trailing ones. read -d consumes readlink's NUL delimiter without
			# altering any byte of the target itself.
			if ! IFS= read -r -d '' target < <(readlink -z -- "$held"); then
				preserved=1
			else
				case "$target" in
					/*) target_path=$target ;;
					*) target_path=$HOME/$target ;;
				esac
			fi
		fi
		if [ "$preserved" -eq 0 ] && realpath -mz -- "$target_path" | cmp -s -- <(realpath -mz -- "$legacy") -; then
			# There is intentionally no second pathname check here. Cleanup runs
			# are serialized above, and the object is inside a random mode-0700
			# directory. After the rename, only the same guest UID or a privileged
			# process can replace it; either can already remove the original alias
			# directly. Rechecking would narrow, not close, that adversarial race
			# while adding complexity to this deletion path.
			rm -- "$held" "$marker"
			continue
		fi
		# A different symlink belongs to the user and is silently restored. A
		# non-symlink means the pathname changed after the initial lstat; restore
		# it too, then report that user data was preserved.
		mv -nT -- "$held" "$alias"
		if [ -e "$held" ] || [ -L "$held" ]; then
			printf 'could not restore guest path %s; preserved object remains at %s\n' "$alias" "$held" >&2
			exit 1
		fi
		rm -- "$marker"
		if [ "$preserved" -eq 1 ]; then
			printf 'cloister-preserved-alias:%s\n' "${alias##*/}"
		fi
	elif [ -e "$alias" ]; then
		printf 'cloister-preserved-alias:%s\n' "${alias##*/}"
    fi
done
rmdir -- "$quarantine" 2>/dev/null || true
trap - EXIT HUP INT TERM
stale=` + shellSingleQuote(root) + `
case "$stale" in
    "$HOME"|"$HOME"/workspaces|"$HOME"/workspaces/*) exit 0 ;;
esac
[ -d "$stale" ] || exit 0
# A real mount at or below the path is a live workspace, not a remnant.
if grep -qF " ${stale}" /proc/mounts 2>/dev/null; then
    exit 0
fi
if [ -z "$(find "$stale" -mindepth 1 -print -quit 2>/dev/null)" ]; then
    exit 0
fi
# A directory left over from a mounted workspace is not necessarily inert. A
# container can bind-mount a path under it, which makes the leftover the live
# storage of a running service rather than an abandoned copy, and advising its
# removal would then be advising data loss.
containers=""
if command -v docker >/dev/null 2>&1; then
    for id in $(docker ps -q 2>/dev/null); do
        sources=$(docker inspect "$id" --format '{{range .Mounts}}{{.Source}}
{{end}}' 2>/dev/null) || continue
        case "$sources" in
            *"$stale"*) containers="$containers $(docker inspect "$id" --format '{{.Name}}' 2>/dev/null | tr -d /)" ;;
        esac
    done
fi
printf '%s\t%s\n' "$(du -sh "$stale" 2>/dev/null | cut -f1) left in $stale" "${containers# }"
`
	// SSHCapture rather than SSHScript: the caller formats this result into a
	// warning, and streaming the guest output as well would print it twice.
	out, err := backend.SSHCapture(profile, script)
	if err != nil {
		if strings.Contains(out, workspaceCleanupLockTimeoutMark) {
			return WorkspaceCleanupReport{}, fmt.Errorf("another guest workspace layout cleanup is still running; skipped alias cleanup after waiting %d seconds", workspaceCleanupLockWaitSeconds)
		}
		return WorkspaceCleanupReport{}, fmt.Errorf("pruning stale workspace aliases: %w", err)
	}
	return parseWorkspaceCleanupReport(out), nil
}

// parseWorkspaceCleanupReport separates fixed alias warnings from the existing
// tab-separated former-mount report. The marker contains only a fixed basename,
// so arbitrary guest paths cannot alter the output protocol.
func parseWorkspaceCleanupReport(out string) WorkspaceCleanupReport {
	var report WorkspaceCleanupReport
	var leftover []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if name, ok := strings.CutPrefix(line, "cloister-preserved-alias:"); ok {
			switch name {
			case "code", "workspace":
				report.PreservedAliases = append(report.PreservedAliases, "~/"+name)
			}
			continue
		}
		if path, ok := strings.CutPrefix(line, "cloister-stranded-alias:"); ok {
			dir, name, found := strings.Cut(path, "/")
			if found && validAliasQuarantineName(dir) && (name == "code" || name == "workspace") {
				report.StrandedAliases = append(report.StrandedAliases, "~/"+path)
			}
			continue
		}
		if path, ok := strings.CutPrefix(line, "cloister-unverified-quarantine:"); ok {
			dir, name, found := strings.Cut(path, "/")
			if found && validAliasQuarantineName(dir) && (name == "code" || name == "workspace") {
				report.UnverifiedQuarantineEntries = append(report.UnverifiedQuarantineEntries, "~/"+path)
			}
			continue
		}
		if strings.TrimSpace(line) != "" {
			leftover = append(leftover, line)
		}
	}
	report.Leftover = parseLeftoverReport(strings.Join(leftover, "\n"))
	return report
}

func validAliasQuarantineName(name string) bool {
	const prefix = ".cloister-alias-quarantine."
	suffix, ok := strings.CutPrefix(name, prefix)
	if !ok || len(suffix) != 6 {
		return false
	}
	for _, char := range suffix {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}

// parseLeftoverReport turns the prune script's tab-separated output into the
// sentence the user reads. An empty description means nothing was left behind.
func parseLeftoverReport(out string) string {
	description, containers, _ := strings.Cut(strings.TrimSpace(out), "\t")
	description = strings.TrimSpace(description)
	containers = strings.TrimSpace(containers)
	if description == "" {
		return ""
	}
	if containers == "" {
		return description
	}
	return description + ", in use by running container(s): " + containers
}

// workspaceRootValue returns the configured workspace root, falling back to the
// profile start directory when the workspace block does not set one.
func workspaceRootValue(p *config.Profile) string {
	if p.Workspace.Root != "" {
		return p.Workspace.Root
	}
	return p.StartDir
}
