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
			Name:    "SSH PATH includes paths.d",
			Check:   `grep -q path_helper /etc/zshenv 2>/dev/null`,
			Install: `sudo -n sh -c 'echo "if [ -x /usr/libexec/path_helper ]; then eval \$(/usr/libexec/path_helper -s); fi" >> /etc/zshenv'`,
		},
		{
			Name:  "DNS uses DHCP default",
			Check: `scutil --dns 2>/dev/null | head -5 | grep -q "$(route -n get default 2>/dev/null | awk '/gateway:/{print $2}')"`,
			Install: `IFACE=$(route -n get default 2>/dev/null | awk '/interface:/{print $2}') && ` +
				`SVC=$(networksetup -listnetworkserviceorder 2>/dev/null | grep -B1 "$IFACE" | head -1 | sed 's/^([0-9]*) //' | sed 's/^ *//' | tr -d '\n') && ` +
				`sudo -n networksetup -setdnsservers "$SVC" empty`,
		},
		{
			Name:  "DNS resolution",
			Check: `host raw.githubusercontent.com >/dev/null 2>&1`,
			Install: `echo "DNS resolution failed. Check network connectivity."`,
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
			Check:   `which brew >/dev/null 2>&1`,
			Install: `NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`,
		},
		{
			Name:    "Homebrew PATH",
			Check:   `grep -q 'brew shellenv' ~/.zprofile 2>/dev/null`,
			Install: `echo 'eval "$(brew shellenv)"' >> ~/.zprofile`,
		},
		{
			Name:    "Node.js",
			Check:   `which node >/dev/null 2>&1`,
			Install: `brew install node`,
		},
		{
			Name:    "Playwright",
			Check:   `npm list -g playwright 2>/dev/null | grep -q playwright`,
			Install: `npm install -g playwright`,
		},
		{
			Name:    "Playwright Chromium",
			Check:   `npx playwright install --dry-run chromium 2>&1 | grep -qi "already installed" || ls $(npm root -g)/playwright/node_modules/playwright-core/.local-browsers/chromium-* >/dev/null 2>&1`,
			Install: `PLAYWRIGHT_BROWSERS_PATH=0 npx playwright install chromium`,
		},
		{
			Name:    "Signal CLI",
			Check:   `which signal-cli >/dev/null 2>&1`,
			Install: `brew install signal-cli`,
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

// BasePreflightSteps returns preflight checks for the base image.
func BasePreflightSteps() []Step {
	return PreflightSteps()
}

// BaseSetupSteps returns the core system setup steps for the base image.
// The sudo step uses echo|sudo -S because NOPASSWD may not exist yet.
// All other steps use sudo -n because NOPASSWD is active after step 1.
func BaseSetupSteps() []Step {
	return []Step{
		{
			Name:    "passwordless sudo",
			Check:   `sudo -n cat /etc/sudoers.d/lume 2>/dev/null | grep -q NOPASSWD`,
			Install: `echo lume | sudo -S sh -c 'echo "lume ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/lume && chmod 0440 /etc/sudoers.d/lume'`,
		},
		{
			Name:  "auto-login",
			Check: `sudo -n sysadminctl -autologin status 2>&1 | grep -q lume`,
			Install: `sudo -n sysadminctl -autologin set -userName lume -password lume 2>/dev/null; ` +
				`perl -e 'my @k=(125,137,82,35,210,188,221,234,163,185,31);my @p=unpack(q{C*},q{lume});my @e;for my $i(0..$#p){$e[$i]=$p[$i]^$k[$i%scalar@k]}my $r=scalar@e%12;push@e,(0)x(12-$r) if $r;open my $f,q{>},q{/tmp/kcp};binmode $f;print $f pack(q{C*},@e);close $f' && ` +
				`sudo -n cp /tmp/kcp /etc/kcpassword && sudo -n chmod 600 /etc/kcpassword && rm /tmp/kcp`,
		},
		{
			Name:    "SSH enabled",
			Check:   `sudo -n systemsetup -getremotelogin 2>/dev/null | grep -qi on`,
			Install: `echo lume | sudo -S systemsetup -setremotelogin on 2>/dev/null`,
		},
		{
			Name:    "SSH PATH includes paths.d",
			Check:   `grep -q path_helper /etc/zshenv 2>/dev/null`,
			Install: `sudo -n sh -c 'echo "if [ -x /usr/libexec/path_helper ]; then eval \$(/usr/libexec/path_helper -s); fi" >> /etc/zshenv'`,
		},
	}
}

// BaseHardeningSteps returns power management, screensaver, and system
// defaults hardening for the base image.
func BaseHardeningSteps() []Step {
	return []Step{
		{
			Name:    "display and system sleep disabled",
			Check:   `sudo -n pmset -g custom 2>/dev/null | awk '/displaysleep/{d=$2} /^ *sleep /{s=$2} END{exit (d==0 && s==0) ? 0 : 1}'`,
			Install: `sudo -n pmset -a displaysleep 0 sleep 0`,
		},
		{
			Name:    "screensaver disabled",
			Check:   `defaults -currentHost read com.apple.screensaver idleTime 2>/dev/null | grep -qx 0`,
			Install: `defaults -currentHost write com.apple.screensaver idleTime -int 0`,
		},
		{
			Name:  "password after sleep disabled",
			Check: `defaults -currentHost read com.apple.screensaver askForPassword 2>/dev/null | grep -qx 0`,
			Install: `defaults -currentHost write com.apple.screensaver askForPassword -int 0 && ` +
				`defaults -currentHost write com.apple.screensaver askForPasswordDelay -int 0`,
		},
		{
			Name:    "auto-logout disabled",
			Check:   `sudo -n defaults read /Library/Preferences/.GlobalPreferences com.apple.autologout.AutoLogOutDelay 2>/dev/null | grep -qx 0`,
			Install: `sudo -n defaults write /Library/Preferences/.GlobalPreferences com.apple.autologout.AutoLogOutDelay -int 0`,
		},
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
			Name:  "software update policy",
			Check: `sudo -n defaults read /Library/Preferences/com.apple.SoftwareUpdate AutomaticallyInstallMacOSUpdates 2>/dev/null | grep -qx 0`,
			Install: `sudo -n defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticCheckEnabled -bool true && ` +
				`sudo -n defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticDownload -bool true && ` +
				`sudo -n defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticallyInstallMacOSUpdates -bool false`,
		},
	}
}

// DaemonStep returns the step for installing the OpenClaw gateway daemon.
func DaemonStep() Step {
	return Step{
		Name:    "OpenClaw daemon",
		Check:   `launchctl list 2>/dev/null | grep -q ai.openclaw.gateway`,
		Install: `export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH" && openclaw onboard --non-interactive --accept-risk --install-daemon --gateway-bind loopback --skip-channels`,
	}
}

// OllamaProviderStep registers the host Ollama instance as an OpenClaw
// provider. Detects the host IP from the VM's default gateway (the Lume
// bridge) and writes the provider config to openclaw.json.
func OllamaProviderStep() Step {
	return Step{
		Name:  "OpenClaw Ollama provider",
		Check: `grep -q '"ollama"' ~/.openclaw/openclaw.json 2>/dev/null && curl -sf http://$(route -n get default 2>/dev/null | awk '/gateway:/{print $2}'):11434/api/tags >/dev/null 2>&1`,
		Install: `export HOST_IP=$(route -n get default 2>/dev/null | awk '/gateway:/{print $2}') && ` +
			`export OC_CONFIG="$HOME/.openclaw/openclaw.json" && ` +
			`python3 -c "
import json, os
cfg_path = os.environ['OC_CONFIG']
with open(cfg_path) as f:
    cfg = json.load(f)
host_ip = os.environ['HOST_IP']
cfg.setdefault('models', {}).setdefault('providers', {})['ollama'] = {'baseUrl': 'http://' + host_ip + ':11434', 'apiKey': 'ollama-local', 'api': 'ollama', 'models': []}
cfg.setdefault('auth', {}).setdefault('profiles', {})['ollama:default'] = {'provider': 'ollama', 'mode': 'api_key'}
cfg.setdefault('agents', {}).setdefault('defaults', {}).setdefault('model', {})['primary'] = 'ollama/qwen3:32b'
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
print('Ollama provider registered at http://' + host_ip + ':11434')
"`,
	}
}

// NodeHostStep returns the step for installing the OpenClaw headless node
// host service. The node host connects to the local gateway and provides
// system.run and system.which capabilities for agent tool execution.
func NodeHostStep() Step {
	return Step{
		Name:    "OpenClaw node host",
		Check:   `launchctl list 2>/dev/null | grep -q ai.openclaw.node`,
		Install: `export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH" && openclaw node install`,
	}
}

// BaseUserSteps creates a dedicated openclaw user with scoped sudo.
// Reduces blast radius if the agent is compromised — attacker gets
// openclaw's limited permissions, not root.
func BaseUserSteps() []Step {
	return []Step{
		{
			Name:    "openclaw user exists",
			Check:   `id openclaw >/dev/null 2>&1`,
			Install: `sudo -n sysadminctl -addUser openclaw -password "$(openssl rand -base64 32)" -shell /bin/zsh && sudo -n dseditgroup -o edit -a openclaw -t user staff`,
		},
		{
			Name:    "openclaw scoped sudo",
			Check:   `sudo -n test -f /etc/sudoers.d/openclaw`,
			Install: `sudo -n sh -c 'echo "openclaw ALL=(ALL) NOPASSWD: /usr/bin/killall, /usr/bin/pkill, /usr/sbin/softwareupdate" > /etc/sudoers.d/openclaw && chmod 0440 /etc/sudoers.d/openclaw'`,
		},
		{
			Name:    "openclaw SSH directory",
			Check:   `sudo -n test -d /Users/openclaw/.ssh`,
			Install: `sudo -n mkdir -p /Users/openclaw/.ssh && sudo -n chmod 700 /Users/openclaw/.ssh && sudo -n chown openclaw:staff /Users/openclaw/.ssh`,
		},
	}
}
