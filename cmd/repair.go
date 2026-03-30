package cmd

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	linuxprov "github.com/ekovshilovsky/cloister/internal/provision/linux"
	macosprov "github.com/ekovshilovsky/cloister/internal/provision/macos"
	"github.com/ekovshilovsky/cloister/internal/vm"
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

// runLumeRepairPhase iterates over a set of steps, using lume ssh to check
// each step's condition and applying the install command if the check fails.
// Returns true if all steps pass after the phase completes.
func runLumeRepairPhase(steps []macosprov.Step, vm string) bool {
	allOK := true
	for _, s := range steps {
		if exec.Command("lume", "ssh", vm, "--", s.Check).Run() == nil {
			fmt.Printf("  %s: OK\n", s.Name)
			continue
		}
		fmt.Printf("  %s: MISSING — fixing... ", s.Name)
		lumeSSH(vm, s.Install)
		if exec.Command("lume", "ssh", vm, "--", s.Check).Run() == nil {
			fmt.Println("fixed")
		} else {
			fmt.Println("FAILED")
			allOK = false
		}
	}
	return allOK
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
	ok1 := runLumeRepairPhase(macosprov.BaseSetupSteps(), vmlume.BaseImageName)
	ok2 := runLumeRepairPhase(macosprov.BaseHardeningSteps(), vmlume.BaseImageName)
	ok3 := runLumeRepairPhase(macosprov.BaseUserSteps(), vmlume.BaseImageName)

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
	ok4 := runLumeRepairPhase(macosprov.BaseSetupSteps(), vmlume.BaseImageName)
	ok5 := runLumeRepairPhase(macosprov.BaseHardeningSteps(), vmlume.BaseImageName)
	ok6 := runLumeRepairPhase(macosprov.BaseUserSteps(), vmlume.BaseImageName)
	allOK := ok1 && ok2 && ok3 && ok4 && ok5 && ok6

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

	fmt.Printf("Repairing profile %q (backend: %s)...\n", name, p.Backend)

	if strings.EqualFold(p.Backend, "lume") {
		return repairLumeProfile(name, p, backend)
	}
	return repairColimaProfile(name, p, backend)
}

// repairColimaProfile re-runs the Linux provisioning steps for a Colima
// profile with per-step progress reporting. Fails fast on any error.
func repairColimaProfile(name string, p *config.Profile, backend vm.Backend) error {
	// Base tools (git, Node, pnpm, Claude Code, op-forward, cloister-vm).
	fmt.Println("Installing base tools...")
	if err := linuxprov.RunScript(name, "scripts/base.sh", backend); err != nil {
		return fmt.Errorf("base tools: %w", err)
	}
	fmt.Println("  ✓ Base tools installed")

	// Stack scripts (dotnet, web, cloud, etc.).
	for _, stack := range p.Stacks {
		scriptName := fmt.Sprintf("scripts/stack-%s.sh", stack)
		fmt.Printf("Installing %s stack...\n", stack)
		if err := linuxprov.RunScript(name, scriptName, backend); err != nil {
			return fmt.Errorf("%s stack: %w", stack, err)
		}
		fmt.Printf("  ✓ %s stack installed\n", stack)
	}

	// GPG isolation if configured.
	if p.GPGSigning {
		fmt.Println("Setting up GPG isolation...")
		if err := linuxprov.RunScript(name, "scripts/gpg-setup.sh", backend); err != nil {
			return fmt.Errorf("GPG setup: %w", err)
		}
		fmt.Println("  ✓ GPG isolation configured")
	}

	// Redeploy bashrc and VM config.
	engine := &linuxprov.Engine{}
	fmt.Println("Deploying configuration...")
	if err := engine.DeployConfig(name, p, backend); err != nil {
		return fmt.Errorf("config deployment: %w", err)
	}
	fmt.Println("  ✓ Configuration deployed")

	// Read-only mount enforcement.
	fmt.Println("Enforcing read-only mounts...")
	if p.Headless {
		if err := linuxprov.RunScriptWithEnv(name, "scripts/read-only-mounts.sh", "CLOISTER_HEADLESS=1", backend); err != nil {
			return fmt.Errorf("read-only mounts: %w", err)
		}
	} else {
		if err := linuxprov.RunScript(name, "scripts/read-only-mounts.sh", backend); err != nil {
			return fmt.Errorf("read-only mounts: %w", err)
		}
	}
	fmt.Println("  ✓ Read-only mounts enforced")

	fmt.Printf("Repair complete for %q — all steps passed.\n", name)
	return nil
}

// repairLumeProfile runs macOS-specific repair checks for a Lume profile,
// verifying sudo, hostname, preflight, provisioning, hardening, and OpenClaw
// daemon steps.
func repairLumeProfile(name string, p *config.Profile, backend vm.Backend) error {
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

	// OpenClaw daemon + node host
	if p.Agent != nil && p.Agent.Type == "openclaw" {
		for _, step := range []macosprov.Step{macosprov.DaemonStep(), macosprov.OllamaProviderStep(), macosprov.NodeHostStep()} {
			if sshOK(step.Check) {
				fmt.Printf("  %s: OK\n", step.Name)
			} else {
				fmt.Printf("  %s: MISSING — fixing...\n", step.Name)
				ssh(step.Install)
				if sshOK(step.Check) {
					fmt.Printf("    %s: fixed\n", step.Name)
				} else {
					fmt.Printf("    %s: FAILED\n", step.Name)
					allOK = false
				}
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
