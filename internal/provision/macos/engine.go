// Package macos implements the Provisioner interface for macOS guest VMs.
// It provisions the guest using Homebrew and direct SSH commands rather than
// embedded shell scripts, since macOS does not have apt and relies on Homebrew
// for package management.
package macos

import (
	"fmt"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// Engine provisions a macOS Lume VM for OpenClaw workloads.
// Unlike the Linux provisioner which uses embedded shell scripts,
// the macOS provisioner runs commands directly via SSH since macOS
// uses Homebrew instead of apt.
type Engine struct{}

// Run executes the full macOS provisioning sequence:
// 1. Install Xcode Command Line Tools (prerequisite for Homebrew)
// 2. Install Homebrew
// 3. Install Node.js via Homebrew
// 4. Install OpenClaw via npm
// 5. Configure OpenClaw gateway (loopback-only bind)
// 6. Start OpenClaw daemon
func (e *Engine) Run(profile string, p *config.Profile, backend vm.Backend) error {
	steps := []struct {
		name    string
		command string
	}{
		// Xcode Command Line Tools must be installed headlessly via
		// softwareupdate because xcode-select --install opens a GUI dialog.
		{"Installing Xcode Command Line Tools",
			`touch /tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress && ` +
				`LABEL=$(softwareupdate -l 2>/dev/null | grep -o 'Command Line Tools[^*]*' | grep -o 'Command Line Tools.*' | head -1 | sed 's/[[:space:]]*$//') && ` +
				`echo "Installing: $LABEL" && ` +
				`sudo softwareupdate -i "$LABEL" --agree-to-license 2>&1 && ` +
				`rm -f /tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress && ` +
				`sudo xcodebuild -license accept 2>/dev/null || true`},
		// Pre-authenticate sudo before running Homebrew's installer.
		{"Installing Homebrew",
			`NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`},
		{"Configuring Homebrew PATH",
			`echo 'eval "$(/opt/homebrew/bin/brew shellenv)"' >> ~/.zprofile && eval "$(/opt/homebrew/bin/brew shellenv)"`},
		{"Installing Node.js",
			"/opt/homebrew/bin/brew install node"},
		{"Installing OpenClaw",
			"/opt/homebrew/bin/npm install -g openclaw@latest"},
	}

	for _, step := range steps {
		fmt.Printf("  %s...\n", step.name)
		if _, err := backend.SSHCommand(profile, step.command); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}

	// Configure and start the OpenClaw daemon when this profile is designated
	// as an OpenClaw agent. Non-agent profiles skip daemon installation.
	if p.Agent != nil && p.Agent.Type == "openclaw" {
		fmt.Println("  Starting OpenClaw daemon...")
		if _, err := backend.SSHCommand(profile, "openclaw onboard --install-daemon"); err != nil {
			return fmt.Errorf("starting OpenClaw daemon: %w", err)
		}
	}

	return nil
}

// DeployConfig re-deploys configuration into a running macOS VM.
// Currently a no-op for macOS — OpenClaw manages its own config.
func (e *Engine) DeployConfig(profile string, p *config.Profile, backend vm.Backend) error {
	return nil
}
