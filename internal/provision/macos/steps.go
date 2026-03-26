package macos

// Step defines a provisioning step with a check to detect if it's already
// done and an install command to apply it. Used by both the provisioner
// (engine.go) which runs install unconditionally, and the repair command
// which checks first and skips what's already configured.
type Step struct {
	Name    string
	Check   string
	Install string
}

// ProvisioningSteps returns the ordered provisioning steps for a macOS VM.
// The steps assume passwordless sudo is already configured (handled by the
// base image's post_ssh_commands or by repair's bootstrap step).
func ProvisioningSteps() []Step {
	return []Step{
		{
			Name:    "Xcode Command Line Tools",
			Check:   `xcode-select -p >/dev/null 2>&1`,
			Install: `touch /tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress && ` +
				`LABEL=$(softwareupdate -l 2>/dev/null | grep -o 'Command Line Tools[^*]*' | grep -o 'Command Line Tools.*' | head -1 | sed 's/[[:space:]]*$//') && ` +
				`echo "Installing: $LABEL" && ` +
				`sudo softwareupdate -i "$LABEL" --agree-to-license 2>&1 && ` +
				`rm -f /tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress && ` +
				`sudo xcodebuild -license accept 2>/dev/null || true`,
		},
		{
			Name:    "Homebrew",
			Check:   `test -x /opt/homebrew/bin/brew`,
			Install: `NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`,
		},
		{
			Name:    "Homebrew PATH",
			Check:   `grep -q 'brew shellenv' ~/.zprofile 2>/dev/null`,
			Install: `echo 'eval "$(/opt/homebrew/bin/brew shellenv)"' >> ~/.zprofile && eval "$(/opt/homebrew/bin/brew shellenv)"`,
		},
		{
			Name:    "Node.js",
			Check:   `test -x /opt/homebrew/bin/node`,
			Install: `/opt/homebrew/bin/brew install node`,
		},
		{
			Name:    "Docker",
			Check:   `test -x /usr/local/bin/docker`,
			Install: `/opt/homebrew/bin/brew install --cask docker && open -a Docker`,
		},
		{
			Name:    "Docker PATH",
			Check:   `grep -q '/usr/local/bin' ~/.zprofile 2>/dev/null`,
			Install: `echo 'export PATH="/usr/local/bin:$PATH"' >> ~/.zprofile`,
		},
		{
			Name:    "OpenClaw",
			Check:   `test -x ~/.local/bin/openclaw`,
			Install: `rm -f /opt/homebrew/bin/openclaw 2>/dev/null; curl -fsSL https://openclaw.ai/install.sh | bash`,
		},
	}
}

// DaemonStep returns the step for installing the OpenClaw daemon.
func DaemonStep() Step {
	return Step{
		Name:    "OpenClaw daemon",
		Check:   `launchctl list 2>/dev/null | grep -q openclaw`,
		Install: `export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH" && openclaw onboard --non-interactive --accept-risk --install-daemon --gateway-bind loopback --skip-channels --skip-skills --skip-search --skip-health`,
	}
}
