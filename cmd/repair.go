package cmd

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	macosprov "github.com/ekovshilovsky/cloister/internal/provision/macos"
	vmlume "github.com/ekovshilovsky/cloister/internal/vm/lume"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(repairCmd)
	repairCmd.Flags().Bool("base", false, "Repair the shared macOS base image instead of a profile")
}

var repairCmd = &cobra.Command{
	Use:   "repair [profile]",
	Short: "Fix missing configuration on an existing VM without rebuilding",
	Long: `Repair checks an existing VM for missing configuration and applies fixes
in-place. No data is destroyed. Runs the same commands as create, checking
each one first and skipping what's already configured.

Pass --base to repair the shared macOS base image.
Pass a profile name to repair that profile's VM.`,
	Args: func(cmd *cobra.Command, args []string) error {
		base, _ := cmd.Flags().GetBool("base")
		if base {
			return nil
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	RunE: runRepair,
}

func runRepair(cmd *cobra.Command, args []string) error {
	base, _ := cmd.Flags().GetBool("base")
	if base {
		return repairBaseImage()
	}
	return repairProfile(args[0])
}

// lumeSSH runs a command on the named VM using lume's built-in password auth.
func lumeSSH(vmName string, command string) string {
	out, _ := exec.Command("lume", "ssh", vmName, "--", command).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// lumeSSHCheck runs a command and returns true if the output contains the
// expected substring. Separates the command execution from the result
// evaluation so stderr noise from sudo doesn't corrupt the check.
func lumeSSHCheck(vmName string, command string, expect string) bool {
	out := lumeSSH(vmName, command)
	return strings.Contains(out, expect)
}

// waitForSystemReady waits for the VM to be fully operational: IP assigned,
// SSH accepting connections, and the macOS user session initialized (Finder
// and cfprefsd running). All repair checks depend on these services.
func waitForSystemReady(vmName string, timeoutSec int) error {
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)

	fmt.Printf("  Waiting for IP...")
	for time.Now().Before(deadline) {
		out, err := exec.Command("lume", "get", vmName, "--format", "json").CombinedOutput()
		if err == nil {
			s := string(out)
			if strings.Contains(s, `"running"`) && strings.Contains(s, `"ipAddress"`) && !strings.Contains(s, `"ipAddress" : null`) {
				fmt.Println(" OK")
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("VM %s: no IP after %d seconds", vmName, timeoutSec)
	}

	fmt.Printf("  Waiting for SSH...")
	for time.Now().Before(deadline) {
		out, err := exec.Command("lume", "ssh", vmName, "--", "echo ready").CombinedOutput()
		if err == nil && strings.Contains(string(out), "ready") {
			fmt.Println(" OK")
			break
		}
		time.Sleep(3 * time.Second)
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("VM %s: SSH not ready after %d seconds", vmName, timeoutSec)
	}

	fmt.Printf("  Waiting for user session (Finder + cfprefsd)...")
	for time.Now().Before(deadline) {
		out := lumeSSH(vmName, "pgrep -x Finder >/dev/null 2>&1 && pgrep -x cfprefsd >/dev/null 2>&1 && echo READY")
		if strings.Contains(out, "READY") {
			fmt.Println(" OK")
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("VM %s: user session not ready after %d seconds", vmName, timeoutSec)
}

func repairBaseImage() error {
	out, _ := exec.Command("lume", "get", vmlume.BaseImageName, "--format", "json").CombinedOutput()
	if !strings.Contains(string(out), vmlume.BaseImageName) {
		return fmt.Errorf("base image %q does not exist", vmlume.BaseImageName)
	}

	wasRunning := strings.Contains(string(out), `"running"`)
	if !wasRunning {
		fmt.Println("Starting base image...")
		lumeCmd := exec.Command("lume", "run", vmlume.BaseImageName, "--no-display")
		if err := lumeCmd.Start(); err != nil {
			return fmt.Errorf("starting base image: %w", err)
		}
		go func() { _ = lumeCmd.Wait() }()
	}

	if err := waitForSystemReady(vmlume.BaseImageName, 180); err != nil {
		return err
	}

	fmt.Println("Running checks and fixes...")
	runBaseChecks(vmlume.BaseImageName)

	fmt.Println("Rebooting to verify persistence...")
	_ = exec.Command("lume", "stop", vmlume.BaseImageName).Run()
	time.Sleep(3 * time.Second)
	lumeCmd2 := exec.Command("lume", "run", vmlume.BaseImageName, "--no-display")
	if err := lumeCmd2.Start(); err != nil {
		return fmt.Errorf("restarting base image: %w", err)
	}
	go func() { _ = lumeCmd2.Wait() }()

	if err := waitForSystemReady(vmlume.BaseImageName, 180); err != nil {
		return err
	}

	fmt.Println("Verifying after reboot...")
	allOK := runBaseChecks(vmlume.BaseImageName)

	if !wasRunning {
		fmt.Println("Stopping base image...")
		_ = exec.Command("lume", "stop", vmlume.BaseImageName).Run()
	}

	if allOK {
		fmt.Println("Base image repair complete — all checks passed.")
	} else {
		fmt.Println("Base image repair complete — some checks still failing (see above).")
	}
	return nil
}

// runBaseChecks runs all base image checks, applying fixes for anything
// missing. Returns true if all checks pass.
func runBaseChecks(vm string) bool {
	allOK := true

	// 1. Passwordless sudo — bootstrap step, uses echo|sudo -S since
	//    NOPASSWD may not exist yet.
	if checkSudo(vm) {
		fmt.Println("  passwordless sudo: OK")
	} else {
		fmt.Print("  passwordless sudo: MISSING — fixing... ")
		lumeSSH(vm, `echo lume | sudo -S sh -c 'echo "lume ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/lume && chmod 0440 /etc/sudoers.d/lume' 2>/dev/null`)
		if checkSudo(vm) {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}

	// All subsequent steps use sudo -n since NOPASSWD should be active.

	// 2. Auto-login
	if checkAutoLogin(vm) {
		fmt.Println("  auto-login: OK")
	} else {
		fmt.Print("  auto-login: MISSING — fixing... ")
		lumeSSH(vm, `sudo -n sysadminctl -autologin set -userName lume -password lume 2>/dev/null`)
		lumeSSH(vm, `perl -e 'my @k=(125,137,82,35,210,188,221,234,163,185,31);my @p=unpack(q{C*},q{lume});my @e;for my $i(0..$#p){$e[$i]=$p[$i]^$k[$i%scalar@k]}my $r=scalar@e%12;push@e,(0)x(12-$r) if $r;open my $f,q{>},q{/tmp/kcp};binmode $f;print $f pack(q{C*},@e);close $f'`)
		lumeSSH(vm, `sudo -n cp /tmp/kcp /etc/kcpassword && sudo -n chmod 600 /etc/kcpassword && rm /tmp/kcp`)
		if checkAutoLogin(vm) {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}

	// 3. Power management (sleep/display)
	if checkPmset(vm) {
		fmt.Println("  sleep/display settings: OK")
	} else {
		fmt.Print("  sleep/display settings: MISSING — fixing... ")
		lumeSSH(vm, `sudo -n pmset -a displaysleep 0 sleep 0`)
		if checkPmset(vm) {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}

	// 4. Screensaver
	if checkScreensaver(vm) {
		fmt.Println("  screensaver disabled: OK")
	} else {
		fmt.Print("  screensaver disabled: MISSING — fixing... ")
		lumeSSH(vm, `defaults -currentHost write com.apple.screensaver idleTime -int 0`)
		if checkScreensaver(vm) {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}

	// 5. Password after sleep
	if checkPasswordAfterSleep(vm) {
		fmt.Println("  password after sleep: OK")
	} else {
		fmt.Print("  password after sleep: MISSING — fixing... ")
		lumeSSH(vm, `defaults -currentHost write com.apple.screensaver askForPassword -int 0`)
		lumeSSH(vm, `defaults -currentHost write com.apple.screensaver askForPasswordDelay -int 0`)
		if checkPasswordAfterSleep(vm) {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}

	// 6. Auto-logout
	if checkAutoLogout(vm) {
		fmt.Println("  auto-logout disabled: OK")
	} else {
		fmt.Print("  auto-logout disabled: MISSING — fixing... ")
		lumeSSH(vm, `sudo -n defaults write /Library/Preferences/.GlobalPreferences com.apple.autologout.AutoLogOutDelay -int 0`)
		if checkAutoLogout(vm) {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}

	// 7. SSH (Remote Login)
	if checkSSH(vm) {
		fmt.Println("  SSH enabled: OK")
	} else {
		fmt.Print("  SSH enabled: MISSING — fixing... ")
		lumeSSH(vm, `echo lume | sudo -S systemsetup -setremotelogin on 2>/dev/null`)
		if checkSSH(vm) {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}

	return allOK
}

func checkSudo(vm string) bool {
	out := lumeSSH(vm, `sudo -n cat /etc/sudoers.d/lume 2>/dev/null`)
	return strings.Contains(out, "NOPASSWD")
}

func checkAutoLogin(vm string) bool {
	out := lumeSSH(vm, `sudo -n sysadminctl -autologin status 2>&1`)
	return strings.Contains(out, "lume")
}

func checkPmset(vm string) bool {
	out := lumeSSH(vm, `sudo -n pmset -g custom 2>/dev/null`)
	displaysleep := extractPmsetValue(out, "displaysleep")
	sleep := extractPmsetValue(out, "sleep")
	return displaysleep == "0" && sleep == "0"
}

func extractPmsetValue(output string, key string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key) {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	return ""
}

func checkScreensaver(vm string) bool {
	out := lumeSSH(vm, `defaults -currentHost read com.apple.screensaver idleTime 2>/dev/null`)
	return strings.TrimSpace(out) == "0"
}

func checkPasswordAfterSleep(vm string) bool {
	out := lumeSSH(vm, `defaults -currentHost read com.apple.screensaver askForPassword 2>/dev/null`)
	return strings.TrimSpace(out) == "0"
}

func checkAutoLogout(vm string) bool {
	out := lumeSSH(vm, `sudo -n defaults read /Library/Preferences/.GlobalPreferences com.apple.autologout.AutoLogOutDelay 2>/dev/null`)
	return strings.TrimSpace(out) == "0"
}

func checkSSH(vm string) bool {
	out := lumeSSH(vm, `sudo -n systemsetup -getremotelogin 2>/dev/null`)
	return strings.Contains(strings.ToLower(out), "on")
}

// runRepairPhase iterates over a set of provisioning steps, using check to
// determine whether each step is already applied. If not, run executes the
// install command and the check is repeated to confirm success. Returns true
// if all steps pass after the phase completes.
func runRepairPhase(steps []macosprov.Step, run func(string) string, check func(string) bool) bool {
	allOK := true
	for _, step := range steps {
		if check(step.Check) {
			fmt.Printf("  %s: OK\n", step.Name)
			continue
		}
		fmt.Printf("  %s: MISSING — fixing...\n", step.Name)
		run(step.Install)
		if check(step.Check) {
			fmt.Printf("    %s: fixed\n", step.Name)
		} else {
			fmt.Printf("    %s: FAILED\n", step.Name)
			allOK = false
		}
	}
	return allOK
}

func repairProfile(name string) error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}

	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	if !backend.IsRunning(name) {
		return fmt.Errorf("profile %q is not running — start it first", name)
	}

	fmt.Printf("Repairing profile %q...\n", name)

	ssh := func(cmd string) string {
		out, _ := backend.SSHCommand(name, cmd)
		return out
	}

	sshOK := func(cmd string) bool {
		_, err := backend.SSHCommand(name, cmd)
		return err == nil
	}

	hostname := vmlume.Hostname(name)

	// Sudo bootstrap — must be first, uses echo|sudo -S since NOPASSWD may not exist.
	allOK := true
	if strings.Contains(ssh(`sudo -n cat /etc/sudoers.d/lume 2>/dev/null`), "NOPASSWD") {
		fmt.Println("  passwordless sudo: OK")
	} else {
		fmt.Print("  passwordless sudo: MISSING — fixing... ")
		ssh(`echo lume | sudo -S sh -c 'echo "lume ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/lume && chmod 0440 /etc/sudoers.d/lume' 2>/dev/null`)
		if strings.Contains(ssh(`sudo -n cat /etc/sudoers.d/lume 2>/dev/null`), "NOPASSWD") {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}

	// Hostname
	if strings.TrimSpace(ssh(`scutil --get LocalHostName 2>/dev/null`)) == hostname {
		fmt.Println("  hostname: OK")
	} else {
		fmt.Print("  hostname: MISSING — fixing... ")
		ssh(fmt.Sprintf(`sudo -n scutil --set LocalHostName %s`, hostname))
		ssh(fmt.Sprintf(`sudo -n scutil --set HostName %s`, hostname))
		if strings.TrimSpace(ssh(`scutil --get LocalHostName 2>/dev/null`)) == hostname {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}

	// Preflight
	if !runRepairPhase(macosprov.PreflightSteps(), ssh, sshOK) {
		allOK = false
	}

	// Provisioning
	if !runRepairPhase(macosprov.ProvisioningSteps(), ssh, sshOK) {
		allOK = false
	}

	// Hardening
	if !runRepairPhase(macosprov.HardeningSteps(), ssh, sshOK) {
		allOK = false
	}

	// OpenClaw daemon
	if p.Agent != nil && p.Agent.Type == "openclaw" {
		ds := macosprov.DaemonStep()
		if sshOK(ds.Check) {
			fmt.Printf("  %s: OK\n", ds.Name)
		} else {
			fmt.Printf("  %s: MISSING — fixing...\n", ds.Name)
			ssh(ds.Install)
			if sshOK(ds.Check) {
				fmt.Printf("    %s: fixed\n", ds.Name)
			} else {
				fmt.Printf("    %s: FAILED\n", ds.Name)
				allOK = false
			}
		}
	}

	if allOK {
		fmt.Printf("Repair complete for %q — all checks passed.\n", name)
	} else {
		fmt.Printf("Repair complete for %q — some checks still failing.\n", name)
	}
	return nil
}
