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

// PreflightSteps returns the ordered preflight checks that must pass before
// provisioning begins. These steps validate and repair environmental
// prerequisites such as network connectivity.
func PreflightSteps() []Step {
	return []Step{
		{
			Name:  "DNS resolution",
			Check: `host raw.githubusercontent.com >/dev/null 2>&1`,
			Install: `IFACE=$(route -n get default 2>/dev/null | awk '/interface:/{print $2}') && ` +
				`SVC=$(networksetup -listnetworkserviceorder 2>/dev/null | grep -B1 "$IFACE" | head -1 | sed 's/^([0-9]*) //' | sed 's/^ *//' | tr -d '\n') && ` +
				`sudo networksetup -setdnsservers "$SVC" 1.1.1.1 8.8.8.8 && ` +
				`sleep 2 && host raw.githubusercontent.com >/dev/null 2>&1`,
		},
	}
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
			Name:    "Playwright",
			Check:   `/opt/homebrew/bin/npm list -g playwright 2>/dev/null | grep -q playwright`,
			Install: `/opt/homebrew/bin/npm install -g playwright`,
		},
		{
			Name:    "Playwright Chromium",
			Check:   `test -d ~/.cache/ms-playwright/chromium-*`,
			Install: `/opt/homebrew/bin/playwright install chromium`,
		},
		{
			Name:    "Signal CLI",
			Check:   `test -x /opt/homebrew/bin/signal-cli`,
			Install: `/opt/homebrew/bin/brew install signal-cli`,
		},
		{
			Name:    "OpenClaw",
			Check:   `test -x ~/.local/bin/openclaw`,
			Install: `rm -f /opt/homebrew/bin/openclaw 2>/dev/null; curl -fsSL https://openclaw.ai/install.sh | bash`,
		},
	}
}

// HardeningSteps returns the ordered hardening steps applied after provisioning
// to configure the VM for headless agent workloads. These steps reduce noise
// from system dialogs, disable unnecessary animations, and enforce a
// download-only software update policy.
func HardeningSteps() []Step {
	return []Step{
		{
			Name:    "crash reporter silent mode",
			Check:   `defaults read com.apple.CrashReporter DialogType 2>/dev/null | grep -qx server`,
			Install: `defaults write com.apple.CrashReporter DialogType -string server`,
		},
		{
			Name:    "crash reporter data submission disabled",
			Check:   `defaults read com.apple.CrashReporter ThirdPartyDataSubmit 2>/dev/null | grep -qx 0`,
			Install: `defaults write com.apple.CrashReporter ThirdPartyDataSubmit -bool false`,
		},
		{
			Name:    "software update policy",
			Check:   `sudo -n defaults read /Library/Preferences/com.apple.SoftwareUpdate AutomaticallyInstallMacOSUpdates 2>/dev/null | grep -qx 0`,
			Install: `sudo -n defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticCheckEnabled -bool true && ` +
				`sudo -n defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticDownload -bool true && ` +
				`sudo -n defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticallyInstallMacOSUpdates -bool false`,
		},
		{
			Name:    "dock animations disabled",
			Check:   `defaults read com.apple.dock launchanim 2>/dev/null | grep -qx 0`,
			Install: `defaults write com.apple.dock launchanim -bool false`,
		},
		{
			Name:    "finder desktop icons disabled",
			Check:   `defaults read com.apple.finder CreateDesktop 2>/dev/null | grep -qx 0`,
			Install: `defaults write com.apple.finder CreateDesktop -bool false`,
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
