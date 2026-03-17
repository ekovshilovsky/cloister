package cmd

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// op-forward service constants.
const (
	opForwardHealthURL = "http://127.0.0.1:18340/health"
	opForwardPort      = "18340"

	clipboardHealthURL = "http://127.0.0.1:18339/health"
	clipboardPort      = "18339"

	// pulseAudioTCPModule is the PulseAudio module directive that binds the native
	// protocol to the loopback interface, making it reachable from inside a VM.
	pulseAudioTCPModule = "load-module module-native-protocol-tcp auth-anonymous=1 listen=127.0.0.1"

	pulseDefaultPA = "/opt/homebrew/etc/pulse/default.pa"
)

// setupFlags holds persistent flag state for the setup subcommand.
type setupFlags struct {
	list bool
}

var stf setupFlags

func init() {
	rootCmd.AddCommand(setupCmd)
	setupCmd.Flags().BoolVar(&stf.list, "list", false, "List all available services and exit")
}

var setupCmd = &cobra.Command{
	Use:   "setup <service>",
	Short: "Guided install for optional services",
	Long: `Set up optional host services that enhance the VM experience.
Available: op-forward, audio, clipboard`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSetup,
}

// runSetup dispatches to the appropriate guided-installation flow based on the
// named service argument.
func runSetup(cmd *cobra.Command, args []string) error {
	if stf.list {
		printSetupServiceList()
		return nil
	}

	if len(args) == 0 {
		return cmd.Help()
	}

	switch args[0] {
	case "op-forward":
		return setupOpForward()
	case "clipboard":
		return setupClipboard()
	case "audio":
		return setupAudio()
	default:
		return fmt.Errorf("unknown service %q — available: op-forward, audio, clipboard", args[0])
	}
}

// printSetupServiceList writes a formatted table of all available services to
// stdout. Each row shows the service name and a brief description.
func printSetupServiceList() {
	fmt.Println("Available services:")
	fmt.Println("  clipboard    Clipboard image pasting (cc-clip)")
	fmt.Println("  op-forward   1Password CLI with Touch ID")
	fmt.Println("  audio        Voice dictation (/voice)")
}

// checkHTTPHealth performs a quick HTTP GET to url and returns true when the
// server responds with HTTP 200. A 2-second timeout prevents hanging when the
// service is not reachable.
func checkHTTPHealth(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url) //nolint:noctx
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// promptYesNo prints prompt to stdout and reads a single line. An empty reply
// or "y"/"Y" is treated as yes; anything else returns false.
func promptYesNo(prompt string) bool {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print(prompt)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.TrimSpace(line)
	return line == "" || strings.EqualFold(line, "y")
}

// runCommandInteractive executes cmd with its stdout and stderr connected to
// the current process so the user can observe progress in real time.
func runCommandInteractive(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// ---- op-forward ----

// setupOpForward guides the user through installing and starting the op-forward
// service, which proxies `op` CLI calls from inside VMs to the Mac host so that
// Touch ID authentication works transparently.
func setupOpForward() error {
	fmt.Println("Setting up 1Password CLI forwarding...")
	fmt.Println()

	// Check whether op-forward is already running before offering any install steps.
	if checkHTTPHealth(opForwardHealthURL) {
		fmt.Println("  ✓ op-forward is already running")
		return nil
	}

	fmt.Println("  This installs op-forward, which forwards 'op' commands from VMs to")
	fmt.Println("  your Mac where Touch ID handles authentication.")
	fmt.Println()
	fmt.Println("  Step 1: Install op-forward")
	fmt.Println("    brew install ekovshilovsky/tap/op-forward")
	fmt.Println()
	fmt.Println("  Step 2: Install as a persistent service")
	fmt.Println("    op-forward service install")
	fmt.Println()

	if !isInteractive() {
		return nil
	}

	if !promptYesNo("Run these commands now? [Y/n]: ") {
		fmt.Println("Skipped. Run the commands above manually when ready.")
		return nil
	}

	fmt.Println()
	fmt.Println("Running: brew install ekovshilovsky/tap/op-forward")
	if err := runCommandInteractive("brew", "install", "ekovshilovsky/tap/op-forward"); err != nil {
		return fmt.Errorf("installing op-forward: %w", err)
	}

	fmt.Println()
	fmt.Println("Running: op-forward service install")
	if err := runCommandInteractive("op-forward", "service", "install"); err != nil {
		return fmt.Errorf("installing op-forward service: %w", err)
	}

	// Allow the service a moment to start before checking health.
	time.Sleep(2 * time.Second)

	if checkHTTPHealth(opForwardHealthURL) {
		fmt.Printf("\n  ✓ op-forward is running on :%s\n", opForwardPort)
		fmt.Println("  Tunnels will be established automatically when you enter a profile.")
	} else {
		fmt.Println("\n  ⚠ op-forward service was installed but health check did not pass.")
		fmt.Println("  Try running 'op-forward service install' again or check the logs.")
	}

	return nil
}

// ---- clipboard ----

// setupClipboard guides the user through installing cc-clip, the host-side
// daemon that exposes clipboard read/write over a local HTTP socket on port
// 18339.
func setupClipboard() error {
	fmt.Println("Setting up clipboard forwarding...")
	fmt.Println()

	if checkHTTPHealth(clipboardHealthURL) {
		fmt.Println("  ✓ cc-clip (clipboard) is already running")
		return nil
	}

	fmt.Println("  This installs cc-clip, which exposes your Mac clipboard to VMs so that")
	fmt.Println("  image and text paste works seamlessly across the VM boundary.")
	fmt.Println()
	fmt.Println("  Step 1: Install cc-clip")
	fmt.Println("    brew install ShunmeiCho/tap/cc-clip")
	fmt.Println()
	fmt.Println("  Step 2: Install as a persistent service")
	fmt.Println("    cc-clip service install")
	fmt.Println()

	if !isInteractive() {
		return nil
	}

	if !promptYesNo("Run these commands now? [Y/n]: ") {
		fmt.Println("Skipped. Run the commands above manually when ready.")
		return nil
	}

	fmt.Println()
	fmt.Println("Running: brew install ShunmeiCho/tap/cc-clip")
	if err := runCommandInteractive("brew", "install", "ShunmeiCho/tap/cc-clip"); err != nil {
		return fmt.Errorf("installing cc-clip: %w", err)
	}

	fmt.Println()
	fmt.Println("Running: cc-clip service install")
	if err := runCommandInteractive("cc-clip", "service", "install"); err != nil {
		return fmt.Errorf("installing cc-clip service: %w", err)
	}

	time.Sleep(2 * time.Second)

	if checkHTTPHealth(clipboardHealthURL) {
		fmt.Printf("\n  ✓ cc-clip is running on :%s\n", clipboardPort)
		fmt.Println("  Tunnels will be established automatically when you enter a profile.")
	} else {
		fmt.Println("\n  ⚠ cc-clip was installed but health check did not pass.")
		fmt.Println("  Try running 'cc-clip service install' again or check the logs.")
	}

	return nil
}

// ---- audio ----

// setupAudio guides the user through installing PulseAudio via Homebrew,
// enabling the TCP module so VMs can connect, selecting a default microphone,
// and restarting the PulseAudio service.
func setupAudio() error {
	fmt.Println("Setting up PulseAudio for voice dictation...")
	fmt.Println()

	// Step 1: verify that PulseAudio is installed.
	if !isPulseAudioInstalled() {
		fmt.Println("  PulseAudio is not installed.")
		fmt.Println()
		fmt.Println("  Install it with:")
		fmt.Println("    brew install pulseaudio")
		fmt.Println()

		if !isInteractive() {
			return nil
		}

		if !promptYesNo("Run this now? [Y/n]: ") {
			fmt.Println("Skipped. Install PulseAudio manually and re-run 'cloister setup audio'.")
			return nil
		}

		fmt.Println()
		fmt.Println("Running: brew install pulseaudio")
		if err := runCommandInteractive("brew", "install", "pulseaudio"); err != nil {
			return fmt.Errorf("installing pulseaudio: %w", err)
		}
	}

	fmt.Println("  ✓ PulseAudio is installed")

	// Step 2: ensure the TCP module directive exists in default.pa so that VMs
	// can connect over the loopback interface without authentication.
	if err := ensurePulseAudioTCPModule(); err != nil {
		return fmt.Errorf("configuring PulseAudio TCP module: %w", err)
	}

	fmt.Println("  ✓ PulseAudio TCP module enabled")

	// Step 3: enumerate available audio sources and let the user choose a
	// default microphone.
	if err := selectDefaultMicrophone(); err != nil {
		// Non-fatal: microphone selection can be skipped; PulseAudio will use
		// its own default.
		fmt.Fprintf(os.Stderr, "  warning: could not set default microphone: %v\n", err)
	}

	// Step 4: restart PulseAudio so the TCP module change takes effect.
	fmt.Println()
	fmt.Println("  Restarting PulseAudio...")
	if err := runCommandInteractive("brew", "services", "restart", "pulseaudio"); err != nil {
		return fmt.Errorf("restarting pulseaudio service: %w", err)
	}

	fmt.Println()
	fmt.Println("  ✓ PulseAudio is configured for VM audio passthrough.")
	fmt.Println("  Tunnels will be established automatically when you enter a profile.")
	return nil
}

// isPulseAudioInstalled returns true when `brew list pulseaudio` exits with
// status 0, indicating PulseAudio has been installed via Homebrew.
func isPulseAudioInstalled() bool {
	err := exec.Command("brew", "list", "pulseaudio").Run()
	return err == nil
}

// ensurePulseAudioTCPModule checks whether the TCP module load directive is
// already present in the PulseAudio configuration file and appends it if not.
func ensurePulseAudioTCPModule() error {
	data, err := os.ReadFile(pulseDefaultPA)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", pulseDefaultPA, err)
	}

	if strings.Contains(string(data), "module-native-protocol-tcp") {
		// Directive is already present; nothing to do.
		return nil
	}

	// Append the directive on a new line.
	f, err := os.OpenFile(pulseDefaultPA, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s for writing: %w", pulseDefaultPA, err)
	}
	defer f.Close()

	_, err = fmt.Fprintf(f, "\n# Added by cloister setup audio — allows VM clients to connect over loopback.\n%s\n", pulseAudioTCPModule)
	return err
}

// pulseSource represents a single PulseAudio audio source (microphone).
type pulseSource struct {
	name        string
	description string
}

// listPulseSources invokes `pactl list sources` and parses the output to
// extract the name and description of each source. Only real hardware sources
// are included; the loopback monitor sources are omitted to avoid confusion.
func listPulseSources() ([]pulseSource, error) {
	out, err := exec.Command("pactl", "list", "sources").Output()
	if err != nil {
		return nil, fmt.Errorf("listing PulseAudio sources: %w", err)
	}

	var sources []pulseSource
	var current pulseSource

	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "Name:") {
			// Flush the previous source when we encounter a new Name block.
			if current.name != "" && !strings.HasSuffix(current.name, ".monitor") {
				sources = append(sources, current)
			}
			current = pulseSource{name: strings.TrimSpace(strings.TrimPrefix(trimmed, "Name:"))}
			continue
		}

		if strings.HasPrefix(trimmed, "Description:") {
			current.description = strings.TrimSpace(strings.TrimPrefix(trimmed, "Description:"))
		}
	}

	// Flush the final block.
	if current.name != "" && !strings.HasSuffix(current.name, ".monitor") {
		sources = append(sources, current)
	}

	return sources, nil
}

// selectDefaultMicrophone enumerates available PulseAudio sources, presents
// them to the user as a numbered list, and calls `pactl set-default-source`
// with the chosen source. It also persists the selection to default.pa.
func selectDefaultMicrophone() error {
	sources, err := listPulseSources()
	if err != nil {
		return err
	}

	if len(sources) == 0 {
		fmt.Println("  No microphones detected; skipping default mic selection.")
		return nil
	}

	fmt.Println()
	fmt.Println("  Available microphones:")
	for i, s := range sources {
		desc := s.description
		if desc == "" {
			desc = s.name
		}
		fmt.Printf("    [%d] %s\n", i+1, desc)
	}
	fmt.Println()

	if !isInteractive() {
		return nil
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("  Select default microphone [1-%d] (Enter to skip): ", len(sources))
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading microphone selection: %w", err)
	}

	line = strings.TrimSpace(line)
	if line == "" {
		fmt.Println("  Skipped microphone selection.")
		return nil
	}

	var idx int
	if _, err := fmt.Sscan(line, &idx); err != nil || idx < 1 || idx > len(sources) {
		return fmt.Errorf("invalid selection %q — enter a number between 1 and %d", line, len(sources))
	}

	chosen := sources[idx-1]

	// Apply the selection immediately to the running PulseAudio daemon.
	if err := exec.Command("pactl", "set-default-source", chosen.name).Run(); err != nil {
		return fmt.Errorf("setting default source %q: %w", chosen.name, err)
	}

	// Persist the selection to default.pa so it survives a PulseAudio restart.
	if err := persistDefaultSource(chosen.name); err != nil {
		// Non-fatal: the live change succeeded; a warning is sufficient.
		fmt.Fprintf(os.Stderr, "  warning: could not persist default source to %s: %v\n", pulseDefaultPA, err)
	}

	fmt.Printf("  ✓ Default microphone set to: %s\n", chosen.description)
	return nil
}

// persistDefaultSource appends a `set-default-source` directive to default.pa
// if one for the given source is not already present.
func persistDefaultSource(sourceName string) error {
	directive := fmt.Sprintf("set-default-source %s", sourceName)

	data, err := os.ReadFile(pulseDefaultPA)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", pulseDefaultPA, err)
	}

	if strings.Contains(string(data), directive) {
		return nil
	}

	f, err := os.OpenFile(pulseDefaultPA, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s for writing: %w", pulseDefaultPA, err)
	}
	defer f.Close()

	_, err = fmt.Fprintf(f, "\n# Added by cloister setup audio — default microphone selection.\n%s\n", directive)
	return err
}

// isInteractive returns true when stdout is connected to a terminal, allowing
// the setup flows to conditionally offer interactive prompts.
func isInteractive() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
