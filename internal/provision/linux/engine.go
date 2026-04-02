// Package linux implements the Provisioner interface for Linux guest VMs. All
// provisioning scripts and configuration templates are embedded at compile time
// so that the cloister binary is fully self-contained. Each public method
// accepts a vm.Backend to decouple the provisioning logic from any specific
// hypervisor.
package linux

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/tunnel"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/ekovshilovsky/cloister/internal/vmconfig"
)

//go:embed scripts/*
var Scripts embed.FS

//go:embed templates/*
var Templates embed.FS

// Engine implements provision.Provisioner for Linux guest VMs. It embeds all
// provisioning scripts and templates and executes them inside the VM via the
// supplied vm.Backend.
type Engine struct{}

// Run executes the full provisioning sequence for the given profile inside the
// corresponding VM. The sequence is:
//  1. Base tools (git, curl, NVM, pnpm, Claude Code)
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
	// Step 1: Base provisioning installs the common toolset shared by all profiles.
	fmt.Println("Installing base tools...")
	if err := RunScript(profile, "scripts/base.sh", backend); err != nil {
		return fmt.Errorf("base provisioning: %w", err)
	}

	// Step 2: Stack provisioning installs each requested toolchain stack.
	for _, stack := range p.Stacks {
		fmt.Printf("Installing %s stack...\n", stack)
		scriptName := fmt.Sprintf("scripts/stack-%s.sh", stack)
		if err := RunScript(profile, scriptName, backend); err != nil {
			return fmt.Errorf("%s stack: %w", stack, err)
		}
	}

	// Post-provisioning host detection warnings for stack-specific services.
	for _, stack := range p.Stacks {
		if stack == "ollama" {
			printOllamaHostWarning()
		}
	}

	// Step 3: GPG isolation exports the host's signing key and deploys it into
	// a VM-local keyring so that commit signing works without mutating or
	// locking the host keyring.
	if p.GPGSigning {
		fmt.Println("Setting up GPG isolation...")
		if err := e.DeployGPGKeys(profile, backend); err != nil {
			// GPG setup failure is non-fatal: the user can still use the VM
			// without commit signing if the host keyring is unavailable.
			fmt.Printf("Warning: GPG setup: %v\n", err)
		}
	}

	// Step 4: Write the managed bashrc so PATH, environment variables, and the
	// configured start directory are applied for every interactive session.
	if err := deployTemplate(profile, "templates/bashrc.tmpl", "~/.bashrc", bashrcData(profile, p), backend); err != nil {
		return fmt.Errorf("deploying bashrc: %w", err)
	}

	// Step 5: Deploy git identity and signing configuration from the host so
	// commits inside the VM use the same author and GPG signing settings.
	fmt.Println("Deploying git configuration...")
	if err := e.DeployGitConfig(profile, p, backend); err != nil {
		fmt.Printf("Warning: git config: %v\n", err)
	}

	// Step 6: Transfer GitHub CLI authentication from the host so that git
	// credential helpers and gh commands work inside the VM.
	fmt.Println("Deploying GitHub CLI authentication...")
	if err := DeployGHAuth(profile, backend); err != nil {
		fmt.Printf("Warning: gh auth: %v\n", err)
	}

	// Step 7: Deploy VM-side config for the cloister-vm toolkit.
	if err := e.DeployVMConfig(profile, p, backend, tunnel.BuiltinTunnelDefs(), bashrcData(profile, p).StartDir); err != nil {
		fmt.Printf("Warning: deploying VM config: %v\n", err)
	}

	// Step 8: Synchronize plugin index files and settings from the host into
	// the VM with translated paths so Claude Code plugins work correctly.
	fmt.Println("Synchronizing plugin configuration...")
	hostHome, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("Warning: could not determine host home directory: %v\n", err)
	} else {
		if err := SyncPlugins(profile, hostHome, backend); err != nil {
			fmt.Printf("Warning: plugin sync: %v\n", err)
		}
	}

	// Step 9: Agent setup — pull Docker image and install cleanup cron.
	if p.Agent != nil {
		fmt.Println("Setting up agent runtime...")
		if err := RunScriptWithEnv(profile, "scripts/agent-setup.sh",
			fmt.Sprintf("AGENT_IMAGE=%s", p.Agent.Image), backend); err != nil {
			return fmt.Errorf("agent setup: %w", err)
		}
	}

	// Step 10: Re-enforce read-only mounts for sensitive directories. This is
	// best-effort: a failure is logged but does not abort provisioning.
	// For headless profiles, the script also locks down Claude extension
	// directories to prevent lateral movement attacks.
	if p.Headless {
		if err := RunScriptWithEnv(profile, "scripts/read-only-mounts.sh", "CLOISTER_HEADLESS=1", backend); err != nil {
			fmt.Printf("Warning: read-only mount enforcement: %v\n", err)
		}
	} else {
		if err := RunScript(profile, "scripts/read-only-mounts.sh", backend); err != nil {
			fmt.Printf("Warning: read-only mount enforcement: %v\n", err)
		}
	}

	// Step 11: Run any custom hooks the user has placed in their cloister config
	// directory, allowing profile-specific post-provisioning steps.
	runCustomHooks(profile)

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
	return deployTemplate(profile, "templates/bashrc.tmpl", "~/.bashrc", bashrcData(profile, p), backend)
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
	script := fmt.Sprintf("mkdir -p ~/.cloister-vm && cat > ~/.cloister-vm/config.json << 'CLOISTER_EOF'\n%s\nCLOISTER_EOF", string(data))
	_, err = backend.SSHScript(profile, script)
	return err
}

// RunScript reads the named embedded script and executes it inside the VM via
// a non-interactive SSH session on the supplied backend. Exported for use by
// the repair command which runs individual scripts independently.
func RunScript(profile, scriptPath string, backend vm.Backend) error {
	data, err := Scripts.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", scriptPath, err)
	}
	_, err = backend.SSHScript(profile, string(data))
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

// RunScriptWithEnv reads the named embedded script and executes it inside the
// VM with the specified environment variable exported before the script runs.
// Exported for use by the repair command.
func RunScriptWithEnv(profile, scriptPath, envLine string, backend vm.Backend) error {
	script, err := assembleScriptWithEnv(scriptPath, envLine)
	if err != nil {
		return err
	}
	_, err = backend.SSHScript(profile, script)
	return err
}

// deployTemplate renders the named embedded Go template with data and writes
// the result to destPath inside the VM using a heredoc.
func deployTemplate(profile, tmplPath, destPath string, data interface{}, backend vm.Backend) error {
	tmplData, err := Templates.ReadFile(tmplPath)
	if err != nil {
		return err
	}
	tmpl, err := template.New("").Parse(string(tmplData))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}
	// Use a heredoc with a unique sentinel so that arbitrary content (including
	// single quotes) is written verbatim without shell interpretation.
	escaped := fmt.Sprintf("cat > %s << 'CLOISTER_EOF'\n%s\nCLOISTER_EOF", destPath, buf.String())
	_, err = backend.SSHCommand(profile, escaped)
	return err
}

// DeployGPGKeys exports the host's GPG public key and copies private key
// material into a VM-local keyring so that commit signing works without
// mutating or locking the host keyring. Runs entirely from the host side
// to avoid mount path and keyboxd format issues inside the VM.
func (e *Engine) DeployGPGKeys(profile string, backend vm.Backend) error {
	hostHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("determining host home: %w", err)
	}

	// Read the signing key ID from host git config.
	keyIDOut, err := exec.Command("git", "config", "--global", "user.signingkey").Output()
	if err != nil {
		return fmt.Errorf("no signing key configured in host git config")
	}
	keyID := strings.TrimSpace(string(keyIDOut))
	if keyID == "" {
		return fmt.Errorf("host git config user.signingkey is empty")
	}

	// Export the public key from the host (works with keyboxd).
	pubKey, err := exec.Command("gpg", "--armor", "--export", keyID).Output()
	if err != nil || len(pubKey) == 0 {
		return fmt.Errorf("exporting public key %s: %w", keyID, err)
	}

	// Collect private key files from the host.
	privKeysDir := fmt.Sprintf("%s/.gnupg/private-keys-v1.d", hostHome)
	privKeyFiles, err := os.ReadDir(privKeysDir)
	if err != nil {
		return fmt.Errorf("reading private keys: %w", err)
	}

	// Build a script that creates the VM-local keyring directory structure.
	var scriptBuf bytes.Buffer
	scriptBuf.WriteString("#!/bin/bash\nset -euo pipefail\n")
	scriptBuf.WriteString("GPG_LOCAL=\"$HOME/.gnupg-local\"\n")
	scriptBuf.WriteString("mkdir -p \"$GPG_LOCAL/private-keys-v1.d\"\n")
	scriptBuf.WriteString("chmod 700 \"$GPG_LOCAL\" \"$GPG_LOCAL/private-keys-v1.d\"\n")

	// Write gpg-agent.conf.
	scriptBuf.WriteString("cat > \"$GPG_LOCAL/gpg-agent.conf\" << 'AGENT_EOF'\n")
	scriptBuf.WriteString("pinentry-program /usr/bin/pinentry-curses\n")
	scriptBuf.WriteString("default-cache-ttl 86400\n")
	scriptBuf.WriteString("max-cache-ttl 86400\n")
	scriptBuf.WriteString("AGENT_EOF\n")

	// Write each private key file into the VM.
	for _, f := range privKeyFiles {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".key") {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("%s/%s", privKeysDir, f.Name()))
		if err != nil {
			continue
		}
		// Use base64 to transport binary key data safely through the shell.
		encoded := base64Encode(data)
		scriptBuf.WriteString(fmt.Sprintf("echo '%s' | base64 -d > \"$GPG_LOCAL/private-keys-v1.d/%s\"\n", encoded, f.Name()))
		scriptBuf.WriteString(fmt.Sprintf("chmod 600 \"$GPG_LOCAL/private-keys-v1.d/%s\"\n", f.Name()))
	}

	// Import the public key (exported from host, compatible format).
	scriptBuf.WriteString("cat << 'PUBKEY_EOF' | GNUPGHOME=\"$GPG_LOCAL\" gpg --batch --import 2>/dev/null || true\n")
	scriptBuf.Write(pubKey)
	scriptBuf.WriteString("PUBKEY_EOF\n")

	// Read the full fingerprint for ownertrust (short key IDs are not accepted).
	fpOut, err := exec.Command("gpg", "--with-colons", "--fingerprint", keyID).Output()
	if err == nil {
		for _, line := range strings.Split(string(fpOut), "\n") {
			if strings.HasPrefix(line, "fpr:") {
				parts := strings.Split(line, ":")
				if len(parts) >= 10 {
					fingerprint := parts[9]
					scriptBuf.WriteString(fmt.Sprintf("echo '%s:6:' | GNUPGHOME=\"$GPG_LOCAL\" gpg --import-ownertrust 2>/dev/null || true\n", fingerprint))
				}
				break
			}
		}
	}

	if _, err := backend.SSHScript(profile, scriptBuf.String()); err != nil {
		return fmt.Errorf("deploying GPG keys: %w", err)
	}
	return nil
}

// base64Encode encodes binary data to a base64 string for safe transport
// through shell heredocs.
func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
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
	return deployTemplate(profile, "templates/gitconfig.tmpl", "~/.gitconfig", data, backend)
}

// DeployGHAuth transfers the host's GitHub CLI authentication into the VM
// so that git credential helpers and gh CLI commands work without manual login.
// Requires gh to be installed on the host and authenticated.
func DeployGHAuth(profile string, backend vm.Backend) error {
	token, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return fmt.Errorf("reading host gh token: %w (is gh authenticated?)", err)
	}
	tokenStr := strings.TrimSpace(string(token))
	if tokenStr == "" {
		return fmt.Errorf("host gh token is empty")
	}
	script := fmt.Sprintf("echo '%s' | gh auth login --with-token 2>/dev/null", tokenStr)
	_, err = backend.SSHScript(profile, script)
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

	// GPGSigning controls whether GNUPGHOME is redirected to the isolated
	// VM-local keyring created by gpg-setup.sh.
	GPGSigning bool

	// ClaudeLocal enables offline Claude Code by pointing it at the host's
	// Ollama server via the Anthropic Messages API compatibility layer.
	ClaudeLocal bool
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
		Profile:     profile,
		StartDir:    ResolveStartDir(p.StartDir),
		GPGSigning:  p.GPGSigning,
		ClaudeLocal: p.ClaudeLocal,
	}
}

// runCustomHooks executes any user-defined provisioning hooks stored in the
// cloister config directory. Hook files are read from the host filesystem and
// executed inside the VM so users can extend provisioning without forking
// cloister itself.
func runCustomHooks(profile string) {
	dir, err := config.ConfigDir()
	if err != nil {
		return
	}
	// TODO: scan dir for profile-specific and global hook scripts and execute
	// each in turn via runScript.
	_ = dir
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
