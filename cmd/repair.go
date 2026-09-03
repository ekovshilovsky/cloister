package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"cloister.io/internal/config"
	linuxprov "cloister.io/internal/provision/linux"
	macosprov "cloister.io/internal/provision/macos"
	"cloister.io/internal/tunnel"
	"cloister.io/internal/vm"
	vmlume "cloister.io/internal/vm/lume"
	"github.com/spf13/cobra"
)

var repairVerbose bool

func init() {
	rootCmd.AddCommand(repairCmd)
	repairCmd.Flags().Bool("base", false, "Repair the shared macOS base image instead of a profile")
	addVerboseFlag(repairCmd, &repairVerbose)
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

// repairChecks runs a repair's check-and-fix steps through a provisioning
// session and remembers which of them are still failing at the end.
//
// The console carries one line per check while the guest output goes to the run
// log, which is what the Colima repair path already does. A check that could not
// be fixed fails its step, so the end of its output is replayed instead of
// leaving the reader a second command to run before learning anything.
type repairChecks struct {
	session *provisionSession

	// guest runs one command inside the VM under repair. The two repair paths
	// differ only in how they reach it.
	guest func(command string) (string, error)

	repaired []string
	failed   []string
}

// verify runs one named check, applying install when the check does not pass
// and repeating the check to confirm the fix took.
func (c *repairChecks) verify(name, check, install string) {
	c.verifyWith(name, install, func(out io.Writer) bool {
		_, ok := c.record(out, check)
		return ok
	})
}

// verifyWith is verify for a check whose condition is what the guest printed
// rather than whether the command succeeded.
func (c *repairChecks) verifyWith(name, install string, passes func(io.Writer) bool) {
	step := c.session.Step(name)
	out := step.Writer()

	if passes(out) {
		step.Done()
		return
	}
	c.record(out, install)
	if passes(out) {
		c.rememberRepair(name)
		step.Done()
		return
	}
	c.failed = append(c.failed, name)
	step.Fail()
}

// rememberRepair retains the first successful repair of each named condition.
// A later verification pass may need to apply the same fix again after a
// reboot, but the final count describes distinct conditions, not attempts.
func (c *repairChecks) rememberRepair(name string) {
	for _, repaired := range c.repaired {
		if repaired == name {
			return
		}
	}
	c.repaired = append(c.repaired, name)
}

// record runs one guest command and puts everything it produced into the step,
// the error included: the backend carries a diagnostic written to standard
// error there and nowhere else, so dropping it leaves a failed check with
// nothing to show.
func (c *repairChecks) record(out io.Writer, command string) (string, bool) {
	guestOut, err := c.guest(command)
	_, _ = io.WriteString(out, guestOut)
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return guestOut, false
	}
	return guestOut, true
}

// run applies a set of provisioning steps in order.
func (c *repairChecks) run(steps []macosprov.Step) {
	for _, step := range steps {
		c.verify(step.Name, step.Check, step.Install)
	}
}

// runRepairPass starts a fresh verification tally and runs every step group in
// that pass. Repair history is retained for the success summary, but failures
// from an earlier VM state cannot leak into the final exit status.
func runRepairPass(c *repairChecks, groups ...[]macosprov.Step) {
	c.failed = nil
	for _, steps := range groups {
		c.run(steps)
	}
}

// report prints the outcome and returns what the command should exit with.
//
// Any check still failing after its fix attempt fails the command. Repair
// promises that the configuration it checks is in place afterwards, and a check
// that did not take means that promise is unmet. An exit status is one bit, so
// a caller running `cloister repair X && cloister enter X` cannot inspect which
// half succeeded; the only safe reading of a partial repair is "not done". No
// detail is lost by that choice -- the console has already named every check
// and its outcome, and the error names the ones still failing.
func (c *repairChecks) report(subject string) error {
	c.session.printLogPath(os.Stdout, "Log: %s\n")
	if len(c.failed) > 0 {
		return fmt.Errorf("%s: %s still failing after repair: %s",
			subject, countOf(len(c.failed), "check"), strings.Join(c.failed, ", "))
	}
	if len(c.repaired) > 0 {
		fmt.Printf("Repair complete for %s — %s repaired: %s.\n",
			subject, countOf(len(c.repaired), "check"), strings.Join(c.repaired, ", "))
	} else {
		fmt.Printf("Repair complete for %s — all final checks passed.\n", subject)
	}
	return nil
}

// countOf renders a count with its noun, pluralized.
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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

	// Guest output goes to the run log; the console carries progress instead.
	session := startProvisionSession(vmlume.BaseImageName, "repair", repairVerbose)
	defer session.Close()
	checks := &repairChecks{session: session, guest: func(command string) (string, error) {
		out, err := exec.Command("lume", "ssh", vmlume.BaseImageName, "--", command).CombinedOutput()
		return string(out), err
	}}

	fmt.Println("Running checks and fixes...")
	runRepairPass(checks,
		macosprov.BaseSetupSteps(),
		macosprov.BaseHardeningSteps(),
		macosprov.BaseUserSteps(),
	)

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

	// The reboot is the point of this phase: a check that passes only until the
	// VM restarts was never really applied, so every check runs a second time.
	fmt.Println("Verifying after reboot...")
	runRepairPass(checks,
		macosprov.BaseSetupSteps(),
		macosprov.BaseHardeningSteps(),
		macosprov.BaseUserSteps(),
	)

	if !wasRunning {
		fmt.Println("Stopping base image...")
		_ = exec.Command("lume", "stop", vmlume.BaseImageName).Run()
	}

	return checks.report("the base image")
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
	// Guest output goes to the run log; the console carries progress instead.
	session := startProvisionSession(name, "repair", repairVerbose)
	defer session.Close()

	// Base tools (git, GitHub CLI, Node, pnpm, Claude Code, op-forward, cloister-vm).
	step := session.Step("Base tools")
	if err := linuxprov.RunScriptTo(name, "scripts/base.sh", backend, step.Writer()); err != nil {
		step.Fail()
		return fmt.Errorf("base tools: %w", err)
	}
	step.Done()

	// Stack scripts (dotnet, web, cloud, etc.).
	for _, stack := range p.Stacks {
		scriptName := fmt.Sprintf("scripts/stack-%s.sh", stack)
		step := session.Step(stack + " stack")
		if err := linuxprov.RunScriptTo(name, scriptName, backend, step.Writer()); err != nil {
			step.Fail()
			return fmt.Errorf("%s stack: %w", stack, err)
		}
		step.Done()
	}

	// Engine instance for template-based deployments. Out is repointed per
	// step so each step's guest output reaches its own sink; the steps below
	// run in sequence, never concurrently.
	engine := &linuxprov.Engine{}

	// GPG isolation if configured.
	if p.GPGSigning {
		gpgStep := session.Step("GPG isolation")
		engine.Out = gpgStep.Writer()
		if err := engine.DeployGPGKeys(name, backend); err != nil {
			// A profile without signing still has a usable VM, so this
			// reports and continues rather than failing the repair.
			gpgStep.Warn(fmt.Sprintf("GPG setup: %v", err))
		} else {
			gpgStep.Done()
		}
	}

	// Reconcile bashrc and redeploy VM config. The bashrc comparison happens
	// before stale-alias cleanup below so a virtiofs-era file cannot recreate
	// aliases after they have been removed.
	configStep := session.Step("Configuration")
	engine.Out = configStep.Writer()
	bashrcResult, err := engine.EnsureBashrc(name, p, backend)
	if err != nil {
		configStep.Fail()
		return fmt.Errorf("bashrc reconciliation: %w", err)
	}
	if bashrcResult.Changed {
		printBashrcReplacementNotice(os.Stderr, bashrcResult.ReplacedSymlink)
	}
	if err := engine.DeployVMConfig(name, p, backend, tunnel.BuiltinTunnelDefs(), linuxprov.ResolveStartDir(p.StartDir)); err != nil {
		configStep.Fail()
		return fmt.Errorf("VM config deployment: %w", err)
	}
	configStep.Done()

	// Deploy git identity and signing configuration from host.
	gitStep := session.Step("Git configuration")
	engine.Out = gitStep.Writer()
	if err := engine.DeployGitConfig(name, p, backend); err != nil {
		gitStep.Warn(fmt.Sprintf("git config: %v", err))
	} else {
		gitStep.Done()
	}

	// Transfer GitHub CLI authentication from host.
	ghStep := session.Step("GitHub CLI authentication")
	if err := linuxprov.DeployGHAuthTo(name, backend, ghStep.Writer()); err != nil {
		ghStep.Warn(fmt.Sprintf("gh auth: %v", err))
	} else {
		ghStep.Done()
	}

	// Synchronize plugin configuration from host.
	pluginStep := session.Step("Plugin configuration")
	hostHome, err := os.UserHomeDir()
	if err != nil {
		pluginStep.Fail()
		return fmt.Errorf("determining host home: %w", err)
	}
	if err := linuxprov.SyncPlugins(name, hostHome, backend, pluginStep.Writer()); err != nil {
		pluginStep.Fail()
		return fmt.Errorf("plugin sync: %w", err)
	}
	pluginStep.Done()

	// Remnants of a mounted workspace on a profile that now synchronizes.
	if p.UsesManagedWorkspace() {
		pruneStep := session.Step("Stale workspace aliases")
		report, err := engine.PruneWorkspaceAliases(name, p, backend)
		if err != nil {
			pruneStep.Warn(fmt.Sprintf("workspace aliases: %v", err))
		} else {
			if report.HasWarnings() {
				pruneStep.Warn("guest workspace entries were preserved")
			} else {
				pruneStep.Done()
			}
			printWorkspaceCleanupWarnings(os.Stderr, report)
		}
	}

	// Read-only mount enforcement.
	mountStep := session.Step("Read-only mounts")
	if p.Headless {
		err = linuxprov.RunScriptWithEnvTo(name, "scripts/read-only-mounts.sh", "CLOISTER_HEADLESS=1", backend, mountStep.Writer())
	} else {
		err = linuxprov.RunScriptTo(name, "scripts/read-only-mounts.sh", backend, mountStep.Writer())
	}
	if err != nil {
		mountStep.Fail()
		return fmt.Errorf("read-only mounts: %w", err)
	}
	mountStep.Done()

	if path := session.LogPath(); path != "" {
		fmt.Printf("Log: %s\n", path)
	}
	fmt.Println(repairSummary(name, session.Warned()))
	return nil
}

// printBashrcReplacementNotice makes replacement of a differing managed file
// visible. This includes hand edits: ~/.bashrc is owned by Cloister and is
// reconciled to the rendered template on entry and repair.
func printBashrcReplacementNotice(out io.Writer, replacedSymlink bool) {
	fmt.Fprintln(out, "notice: ~/.bashrc differed from Cloister's managed configuration and was replaced")
	if replacedSymlink {
		fmt.Fprintln(out, "notice: ~/.bashrc was a symbolic link; Cloister replaced the link itself and left its target unchanged")
	}
}

// printWorkspaceCleanupWarnings explains every entry that cleanup deliberately
// preserved. Alias paths are fixed guest-home names; the former mount report is
// generated from the configured host path.
func printWorkspaceCleanupWarnings(out io.Writer, report linuxprov.WorkspaceCleanupReport) {
	for _, alias := range report.PreservedAliases {
		fmt.Fprintf(out, "warning: guest path %s is not a symlink; preserving it\n", alias)
	}
	for _, stranded := range report.StrandedAliases {
		fmt.Fprintf(out, "warning: cleanup entry %s could not be restored because its guest-home path is occupied; it remains preserved at this path\n", stranded)
	}
	if report.Leftover == "" {
		return
	}
	fmt.Fprintf(out, "warning: %s\n", report.Leftover)
	fmt.Fprintln(out, "  This is a guest-local directory, not a mount, and not a synchronized")
	fmt.Fprintln(out, "  project. Your projects live under ~/workspaces.")
	if strings.Contains(report.Leftover, "in use by running container") {
		fmt.Fprintln(out, "  It is live storage for the container(s) named above, so leave it in")
		fmt.Fprintln(out, "  place unless you are retiring them.")
	} else {
		fmt.Fprintln(out, "  Check that nothing is using it, then remove it by hand if you no")
		fmt.Fprintln(out, "  longer need what it holds.")
	}
}

// repairSummary is the closing line of a Colima repair.
//
// Several of the steps above report a problem and carry on, because a profile
// whose GPG setup failed still has a usable VM. That is a good reason not to
// fail the command and no reason at all to then say every step passed: the
// summary names them instead. They do not change the exit status, since the
// repair did what it could and the VM is usable, and that is the difference
// between a warning and a failure.
func repairSummary(name string, warned []string) string {
	if len(warned) == 0 {
		return fmt.Sprintf("Repair complete for %q — all steps passed.", name)
	}
	return fmt.Sprintf("Repair complete for %q — %s reported a problem and were left as they are: %s.",
		name, countOf(len(warned), "step"), strings.Join(warned, ", "))
}

// repairLumeProfile runs macOS-specific repair checks for a Lume profile,
// verifying sudo, hostname, preflight, provisioning, hardening, and OpenClaw
// daemon steps.
func repairLumeProfile(name string, p *config.Profile, backend vm.Backend) error {
	if configurable, ok := backend.(interface{ SetVerbose(bool) }); ok {
		configurable.SetVerbose(repairVerbose)
	}

	// Guest output goes to the run log; the console carries progress instead.
	session := startProvisionSession(name, "repair", repairVerbose)
	defer session.Close()

	checks := &repairChecks{session: session, guest: func(command string) (string, error) {
		return backend.SSHCommand(name, command)
	}}

	hostname := vmlume.Hostname(name)

	// Sudo bootstrap — must be first, and uses echo|sudo -S because the
	// NOPASSWD rule it installs is what everything after it depends on. The
	// condition is the content of the sudoers file rather than an exit status,
	// since reading it succeeds whatever it says.
	checks.verifyWith("passwordless sudo",
		`echo lume | sudo -S sh -c 'echo "lume ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/lume && chmod 0440 /etc/sudoers.d/lume' 2>/dev/null`,
		func(out io.Writer) bool {
			sudoers, _ := checks.record(out, `sudo -n cat /etc/sudoers.d/lume 2>/dev/null`)
			return strings.Contains(sudoers, "NOPASSWD")
		})

	// Hostname. Setting it takes two commands, so the fix is a compound one.
	checks.verifyWith("hostname",
		fmt.Sprintf(`sudo -n scutil --set LocalHostName %s && sudo -n scutil --set HostName %s`, hostname, hostname),
		func(out io.Writer) bool {
			current, _ := checks.record(out, `scutil --get LocalHostName 2>/dev/null`)
			return strings.TrimSpace(current) == hostname
		})

	checks.run(macosprov.PreflightSteps())
	checks.run(macosprov.ProvisioningSteps())
	checks.run(macosprov.HardeningSteps())

	if p.Agent != nil && p.Agent.Type == "openclaw" {
		checks.run([]macosprov.Step{macosprov.DaemonStep(), macosprov.OllamaProviderStep(), macosprov.NodeHostStep()})
	}

	return checks.report(fmt.Sprintf("%q", name))
}
