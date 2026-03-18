// Package provision orchestrates the installation of base tools and optional
// toolchain stacks inside a cloister-managed VM. All provisioning scripts and
// configuration templates are embedded at compile time so that the cloister
// binary is fully self-contained and requires no external asset paths.
package provision

import (
	"bytes"
	"embed"
	"fmt"
	"net"
	"text/template"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

//go:embed scripts/*
var Scripts embed.FS

//go:embed templates/*
var Templates embed.FS

// Run executes the full provisioning sequence for the given profile inside the
// corresponding VM. The sequence is:
//  1. Base tools (git, curl, NVM, pnpm, Claude Code)
//  2. Each requested toolchain stack in order
//  3. GPG key isolation (when GPGSigning is enabled)
//  4. Deployment of the managed ~/.bashrc
//  5. Read-only re-mount enforcement for sensitive host-shared directories
//  6. Any custom per-profile provisioning hooks present on the host
func Run(profile string, p *config.Profile) error {
	// Step 1: Base provisioning installs the common toolset shared by all profiles.
	fmt.Println("Installing base tools...")
	if err := runScript(profile, "scripts/base.sh"); err != nil {
		return fmt.Errorf("base provisioning: %w", err)
	}

	// Step 2: Stack provisioning installs each requested toolchain stack.
	for _, stack := range p.Stacks {
		fmt.Printf("Installing %s stack...\n", stack)
		scriptName := fmt.Sprintf("scripts/stack-%s.sh", stack)
		if err := runScript(profile, scriptName); err != nil {
			return fmt.Errorf("%s stack: %w", stack, err)
		}
	}

	// Post-provisioning host detection warnings for stack-specific services.
	for _, stack := range p.Stacks {
		if stack == "ollama" {
			printOllamaHostWarning()
		}
	}

	// Step 3: GPG isolation copies key material into a VM-local keyring so that
	// commit signing works without mutating or locking the host keyring.
	if p.GPGSigning {
		fmt.Println("Setting up GPG isolation...")
		if err := runScript(profile, "scripts/gpg-setup.sh"); err != nil {
			// GPG setup failure is non-fatal: the user can still use the VM
			// without commit signing if the host keyring is unavailable.
			fmt.Printf("Warning: GPG setup issue: %v\n", err)
		}
	}

	// Step 4: Write the managed bashrc so PATH, environment variables, and the
	// configured start directory are applied for every interactive session.
	if err := deployTemplate(profile, "templates/bashrc.tmpl", "~/.bashrc", bashrcData(profile, p)); err != nil {
		return fmt.Errorf("deploying bashrc: %w", err)
	}

	// Step 5: Re-enforce read-only mounts for sensitive directories. This is
	// best-effort: a failure is logged but does not abort provisioning.
	// For headless profiles, the script also locks down Claude extension
	// directories to prevent lateral movement attacks.
	if p.Headless {
		if err := runScriptWithEnv(profile, "scripts/read-only-mounts.sh", "CLOISTER_HEADLESS=1"); err != nil {
			fmt.Printf("Warning: read-only mount enforcement: %v\n", err)
		}
	} else {
		if err := runScript(profile, "scripts/read-only-mounts.sh"); err != nil {
			fmt.Printf("Warning: read-only mount enforcement: %v\n", err)
		}
	}

	// Step 6: Run any custom hooks the user has placed in their cloister config
	// directory, allowing profile-specific post-provisioning steps.
	runCustomHooks(profile)

	return nil
}

// runScript reads the named embedded script and executes it inside the VM via
// a non-interactive SSH session.
func runScript(profile, scriptPath string) error {
	data, err := Scripts.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", scriptPath, err)
	}
	_, err = vm.SSHScript(profile, string(data))
	return err
}

// runScriptWithEnv reads the named embedded script and executes it inside the
// VM with the specified environment variable exported before the script runs.
func runScriptWithEnv(profile, scriptPath, envLine string) error {
	data, err := Scripts.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", scriptPath, err)
	}
	script := fmt.Sprintf("export %s\n%s", envLine, string(data))
	_, err = vm.SSHScript(profile, script)
	return err
}

// deployTemplate renders the named embedded Go template with data and writes
// the result to destPath inside the VM using a heredoc.
func deployTemplate(profile, tmplPath, destPath string, data interface{}) error {
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
	_, err = vm.SSHCommand(profile, escaped)
	return err
}

// bashrcTemplateData holds the values substituted into templates/bashrc.tmpl.
type bashrcTemplateData struct {
	// Profile is the cloister profile name, rendered as a comment header so
	// it is easy to identify which VM a given bashrc belongs to.
	Profile string

	// StartDir is the directory the shell changes into at login. Falls back to
	// ~/Code when the profile does not specify a start directory.
	StartDir string

	// GPGSigning controls whether GNUPGHOME is redirected to the isolated
	// VM-local keyring created by gpg-setup.sh.
	GPGSigning bool
}

// bashrcData constructs the template data for the bashrc template from the
// given profile name and its configuration.
func bashrcData(profile string, p *config.Profile) bashrcTemplateData {
	startDir := p.StartDir
	if startDir == "" {
		startDir = "~/code"
	}
	return bashrcTemplateData{
		Profile:    profile,
		StartDir:   startDir,
		GPGSigning: p.GPGSigning,
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

// printOllamaHostWarning checks whether the host Ollama server is running and
// prints guidance when it is not detected.
func printOllamaHostWarning() {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:11434", 500*time.Millisecond)
	if err != nil {
		fmt.Println("  ⚠ Host Ollama not detected on port 11434.")
		fmt.Println("    Install on your Mac for GPU-accelerated inference: brew install ollama")
		fmt.Println("    The ollama CLI is installed in the VM but has no server to connect to")
		fmt.Println("    until host Ollama is running and the tunnel is forwarded.")
	} else {
		conn.Close()
		fmt.Println("  ✓ Host Ollama detected — will be tunneled into VM on entry")
	}
}
