package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/agent"
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/profile"
	"github.com/ekovshilovsky/cloister/internal/provision"
	"github.com/ekovshilovsky/cloister/internal/tunnel"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

// createFlags holds all user-supplied values for the create command. A
// dedicated struct avoids polluting the package-level namespace with flag
// variables that are only relevant to this subcommand.
type createFlags struct {
	defaults         bool
	memory           int
	startDir         string
	color            string
	stack            string
	gpgSigning       bool
	claudeLocal      bool
	disk             int
	cpu              int
	dotnetVersion    string
	nodeVersion      string
	pythonVersion    string
	goVersion        string
	rustVersion      string
	terraformVersion string
	listOptions      bool
	jsonOutput       bool
	headless         bool
	openclaw         bool
}

var cf createFlags

// createProfileResult is the machine-readable representation of a successfully
// created profile, emitted when --json is set.
type createProfileResult struct {
	Name   string          `json:"name"`
	Config *config.Profile `json:"config"`
}

func init() {
	rootCmd.AddCommand(createCmd)

	f := createCmd.Flags()
	f.BoolVar(&cf.defaults, "defaults", false, "Create the profile using defaults without interactive prompts")
	f.IntVar(&cf.memory, "memory", 0, "VM memory in gigabytes")
	f.StringVar(&cf.startDir, "start-dir", "", "Working directory opened when attaching to the VM")
	f.StringVar(&cf.color, "color", "", "Terminal accent color as a 6-character hex string (e.g. 0a1628)")
	f.StringVar(&cf.stack, "stack", "", "Comma-separated list of toolchain stacks to provision (web,cloud,dotnet,python,go,rust,data)")
	f.BoolVar(&cf.gpgSigning, "gpg-signing", false, "Enable automatic GPG commit-signing inside the VM")
	f.BoolVar(&cf.claudeLocal, "claude-local", false, "Run Claude Code against local Ollama instead of Anthropic's cloud API")
	f.IntVar(&cf.disk, "disk", 0, "VM disk size in gigabytes")
	f.IntVar(&cf.cpu, "cpu", 0, "Number of virtual CPUs assigned to the VM")
	f.StringVar(&cf.dotnetVersion, "dotnet-version", "", "Pin a specific .NET SDK version")
	f.StringVar(&cf.nodeVersion, "node-version", "", "Pin a specific Node.js version")
	f.StringVar(&cf.pythonVersion, "python-version", "", "Pin a specific Python version")
	f.StringVar(&cf.goVersion, "go-version", "", "Pin a specific Go toolchain version")
	f.StringVar(&cf.rustVersion, "rust-version", "", "Pin a specific Rust toolchain version")
	f.StringVar(&cf.terraformVersion, "terraform-version", "", "Pin a specific Terraform CLI version")
	f.BoolVar(&cf.listOptions, "list-options", false, "Print all configurable options and exit")
	f.BoolVar(&cf.jsonOutput, "json", false, "Emit the created profile as JSON instead of human-readable text")
	f.BoolVar(&cf.headless, "headless", false, "Create a headless agent profile (no interactive shell access)")
	f.BoolVar(&cf.openclaw, "openclaw", false, "Configure the profile for OpenClaw (implies --headless, auto-selects stacks)")
}

var createCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new cloister profile",
	Long: `Create a new cloister profile with an isolated VM configuration.

When run without flags the command enters an interactive wizard that
guides you through the available options. Pass --defaults to accept
all defaults non-interactively, or supply individual flags to override
specific fields while using defaults for the rest.`,
	Args: cobra.ExactArgs(1),
	RunE: runCreate,
}

// runCreate is the main handler for the create subcommand.
func runCreate(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Display the option catalogue and exit early when requested.
	if cf.listOptions {
		printListOptions(cmd, cf.jsonOutput)
		return nil
	}

	// Validate the profile name before doing any I/O.
	if err := profile.ValidateName(name); err != nil {
		return err
	}

	// --openclaw implies --headless and --defaults
	if cf.openclaw {
		if cmd.Flags().Changed("headless") && !cf.headless {
			return fmt.Errorf("--openclaw requires headless mode; --headless=false conflicts")
		}
		cf.headless = true
		cf.defaults = true
	}

	// Load the existing configuration so we can detect duplicate profiles.
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if _, exists := cfg.Profiles[name]; exists {
		return fmt.Errorf("profile %q already exists", name)
	}

	// Determine whether any explicit flag was provided so we know whether to
	// enter the interactive wizard.
	anyFlagSet := cmd.Flags().Changed("memory") ||
		cmd.Flags().Changed("start-dir") ||
		cmd.Flags().Changed("color") ||
		cmd.Flags().Changed("stack") ||
		cmd.Flags().Changed("gpg-signing") ||
		cmd.Flags().Changed("claude-local") ||
		cmd.Flags().Changed("disk") ||
		cmd.Flags().Changed("cpu") ||
		cmd.Flags().Changed("dotnet-version") ||
		cmd.Flags().Changed("node-version") ||
		cmd.Flags().Changed("python-version") ||
		cmd.Flags().Changed("go-version") ||
		cmd.Flags().Changed("rust-version") ||
		cmd.Flags().Changed("terraform-version") ||
		cmd.Flags().Changed("headless") ||
		cmd.Flags().Changed("openclaw")

	p := &config.Profile{}

	if cf.defaults || anyFlagSet {
		// Non-interactive path: apply defaults then overlay any explicit flags.
		p.ApplyDefaults()
		applyFlagsToProfile(p, cmd)

		if cf.headless {
			p.Headless = true
		}

		if cf.openclaw {
			p.Stacks = agent.OpenClawStacks()
			p.Agent = agent.OpenClawDefaults()

			// Auto-detect op-forward on host for secure credential injection
			if tunnel.ProbeByName("op-forward") {
				p.TunnelPolicy = config.ResourcePolicy{
					IsSet: true,
					Names: []string{"op-forward"},
				}
			}
		}
	} else {
		// Interactive path: ask the user whether to use defaults or step
		// through each configurable field individually.
		if err := runInteractiveWizard(p, cfg); err != nil {
			return err
		}
	}

	// Auto-assign a palette color when none was provided through any path.
	if p.Color == "" {
		p.Color = profile.AutoColor(len(cfg.Profiles))
	}

	// Validate any stacks the user requested.
	if len(p.Stacks) > 0 {
		if err := profile.ValidateStacks(p.Stacks); err != nil {
			return err
		}
	}
	if len(p.TunnelPolicy.Names) > 0 {
		if err := profile.ValidateTunnelNames(p.TunnelPolicy.Names); err != nil {
			return err
		}
	}
	if len(p.MountPolicy.Names) > 0 {
		if err := profile.ValidateMountNames(p.MountPolicy.Names); err != nil {
			return err
		}
	}

	// Local Claude Code mode requires the ollama stack for the tunnel and CLI.
	if p.ClaudeLocal && !p.HasStack("ollama") {
		return fmt.Errorf("--claude-local requires the ollama stack (--stack ollama)")
	}

	// Resolve and validate the workspace directory before persisting the
	// profile. Any broken profile that slips into config before this check
	// would require manual cleanup to remove.
	home, _ := os.UserHomeDir()
	workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return fmt.Errorf("invalid workspace directory: %w", err)
	}
	if _, err := os.Stat(workspaceDir); err != nil {
		return fmt.Errorf("workspace directory %q is not accessible: %w\nSpecify your workspace with: cloister create %s --start-dir ~/path/to/workspace", workspaceDir, err, name)
	}

	// Persist the new profile only after the workspace is confirmed to be
	// reachable, preventing a broken entry from remaining in config.
	cfg.Profiles[name] = p
	if err := config.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	// Emit output.
	if cf.jsonOutput {
		return printJSON(cmd, name, p)
	}

	cmd.Printf("Profile %q created.\n", name)

	// Start the VM immediately so that provisioning can run without requiring
	// a separate entry step. Defaults must be applied before passing resource
	// values to the VM layer.
	fmt.Printf("Starting %q...\n", name)
	mounts := vm.BuildMounts(home, workspaceDir, p.Stacks, p.MountPolicy, p.Headless)

	// Add agent data directory mount for headless agent profiles
	if p.Agent != nil {
		agentDir, err := agentDataDir(name, p.Agent.Type)
		if err != nil {
			return fmt.Errorf("resolving agent data directory: %w", err)
		}
		os.MkdirAll(agentDir, 0o700)
		mounts = append(mounts, vm.Mount{Location: agentDir, Writable: true})
	}

	p.ApplyDefaults()
	if err := vm.Start(name, p.CPU, p.Memory, p.Disk, mounts, false); err != nil {
		return fmt.Errorf("failed to start environment: %w", err)
	}

	// Run the full provisioning sequence: base tools, requested stacks,
	// GPG isolation, bashrc deployment, and custom hooks.
	fmt.Println("Provisioning environment...")
	if err := provision.Run(name, p); err != nil {
		return fmt.Errorf("provisioning failed: %w", err)
	}

	if p.Agent != nil {
		// Create host-side agent data directory
		agentDir, err := agentDataDir(name, p.Agent.Type)
		if err != nil {
			return fmt.Errorf("creating agent data directory: %w", err)
		}
		if err := os.MkdirAll(agentDir, 0o700); err != nil {
			return fmt.Errorf("creating agent data directory: %w", err)
		}

		fmt.Printf("\nProfile %q created.\n\n", name)
		fmt.Println("Next steps:")
		fmt.Printf("  1. Start the agent:    cloister agent %s start\n", name)
		for _, port := range p.Agent.Ports {
			fmt.Printf("  2. Forward the web UI:  cloister agent %s forward %d\n", name, port)
		}
		fmt.Printf("  3. Open in browser:     http://localhost:%d\n", p.Agent.Ports[0])
		fmt.Println("  4. Complete the onboarding wizard to connect messaging platforms")
		fmt.Printf("  5. Close the forward:   cloister agent %s close\n", name)
		return nil
	}

	fmt.Printf("\nProfile %q ready. Enter with: cloister %s\n", name, name)
	fmt.Println("On first entry, run: claude login")
	return nil
}

// applyFlagsToProfile overlays the values of any flags that were explicitly
// set by the caller onto the supplied profile, leaving defaults in place for
// flags that were not provided.
func applyFlagsToProfile(p *config.Profile, cmd *cobra.Command) {
	if cmd.Flags().Changed("memory") {
		p.Memory = cf.memory
	}
	if cmd.Flags().Changed("start-dir") {
		p.StartDir = cf.startDir
	}
	if cmd.Flags().Changed("color") {
		p.Color = cf.color
	}
	if cmd.Flags().Changed("stack") {
		p.Stacks = parseStacks(cf.stack)
	}
	if cmd.Flags().Changed("gpg-signing") {
		p.GPGSigning = cf.gpgSigning
	}
	if cmd.Flags().Changed("claude-local") {
		p.ClaudeLocal = cf.claudeLocal
	}
	if cmd.Flags().Changed("disk") {
		p.Disk = cf.disk
	}
	if cmd.Flags().Changed("cpu") {
		p.CPU = cf.cpu
	}
	if cmd.Flags().Changed("dotnet-version") {
		p.DotnetVersion = cf.dotnetVersion
	}
	if cmd.Flags().Changed("node-version") {
		p.NodeVersion = cf.nodeVersion
	}
	if cmd.Flags().Changed("python-version") {
		p.PythonVersion = cf.pythonVersion
	}
	if cmd.Flags().Changed("go-version") {
		p.GoVersion = cf.goVersion
	}
	if cmd.Flags().Changed("rust-version") {
		p.RustVersion = cf.rustVersion
	}
	if cmd.Flags().Changed("terraform-version") {
		p.TerraformVersion = cf.terraformVersion
	}
}

// runInteractiveWizard prompts the user for each configurable profile field.
// When ~/code exists, the user is offered a defaults shortcut; when it does
// not, the workspace directory prompt is mandatory and the shortcut is skipped.
func runInteractiveWizard(p *config.Profile, cfg *config.Config) error {
	reader := bufio.NewReader(os.Stdin)

	home, _ := os.UserHomeDir()
	// ResolveWorkspaceDir with an empty startDir always returns the default
	// ~/code path and never errors, so the error is intentionally discarded.
	defaultCodeDir, _ := config.ResolveWorkspaceDir("", home)
	codeExists := false
	if _, err := os.Stat(defaultCodeDir); err == nil {
		codeExists = true
	}

	if codeExists {
		// Offer the one-keystroke defaults shortcut only when the default
		// workspace directory already exists on the host.
		fmt.Print("Use defaults? (4GB RAM, ~/code, auto color) [Y/n]: ")
		answer, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading input: %w", err)
		}
		answer = strings.TrimSpace(answer)

		if answer == "" || strings.EqualFold(answer, "y") {
			p.ApplyDefaults()
			p.Color = profile.AutoColor(len(cfg.Profiles))
			return nil
		}
	} else {
		// ~/code is absent: inform the user and proceed directly to the
		// per-field wizard so they can supply an existing workspace path.
		fmt.Println("~/code not found. Please configure your workspace directory.")
	}

	// Step through each field individually.
	p.ApplyDefaults() // seed from defaults so unmodified fields are not zero

	memory, err := promptInt(reader,
		fmt.Sprintf("Memory in GB [%d]: ", config.DefaultMemory),
		config.DefaultMemory)
	if err != nil {
		return err
	}
	p.Memory = memory

	// Prompt for the workspace directory and validate that the resolved path is
	// accessible before accepting the input. The loop re-prompts the user until
	// a valid, reachable path is provided.
	var startDir string
	prompt := fmt.Sprintf("Start directory [%s]: ", config.DefaultStartDir)
	defaultVal := config.DefaultStartDir
	if !codeExists {
		prompt = "Start directory (required): "
		defaultVal = ""
	}
	for {
		startDir, err = promptString(reader, prompt, defaultVal)
		if err != nil {
			return err
		}
		if startDir == "" {
			fmt.Println("A workspace directory is required.")
			continue
		}
		resolved, resolveErr := config.ResolveWorkspaceDir(startDir, home)
		if resolveErr != nil {
			fmt.Printf("Invalid path: %v. Please try again.\n", resolveErr)
			continue
		}
		if _, statErr := os.Stat(resolved); statErr != nil {
			fmt.Printf("Directory %q does not exist or is not accessible. Please try again.\n", resolved)
			continue
		}
		break
	}
	p.StartDir = startDir

	color, err := promptString(reader,
		fmt.Sprintf("Accent color hex [auto → %s]: ", profile.AutoColor(len(cfg.Profiles))),
		"")
	if err != nil {
		return err
	}
	p.Color = color // empty string is handled by the auto-assign step in runCreate

	stackInput, err := promptString(reader,
		"Stacks (comma-separated: web,cloud,dotnet,python,go,rust,data) [none]: ",
		"")
	if err != nil {
		return err
	}
	if stackInput != "" {
		p.Stacks = parseStacks(stackInput)
	}

	gpgAnswer, err := promptString(reader, "Enable GPG signing? [y/N]: ", "n")
	if err != nil {
		return err
	}
	p.GPGSigning = strings.EqualFold(strings.TrimSpace(gpgAnswer), "y")

	return nil
}

// promptString prints prompt to stdout and reads a single line. When the user
// enters an empty line the supplied defaultVal is returned.
func promptString(r *bufio.Reader, prompt, defaultVal string) (string, error) {
	fmt.Print(prompt)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading input: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultVal, nil
	}
	return line, nil
}

// promptInt prints prompt to stdout and reads a single line, parsing it as an
// integer. An empty line returns defaultVal. An unparseable value returns an
// error.
func promptInt(r *bufio.Reader, prompt string, defaultVal int) (int, error) {
	fmt.Print(prompt)
	line, err := r.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("reading input: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultVal, nil
	}
	n, err := strconv.Atoi(line)
	if err != nil {
		return 0, fmt.Errorf("expected an integer, got %q", line)
	}
	return n, nil
}

// parseStacks splits a comma-separated stack string into a trimmed slice,
// discarding any empty tokens that result from trailing commas or extra spaces.
func parseStacks(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// printListOptions writes the list of all configurable profile fields.
// When --json is set, emits a structured JSON schema for AI-friendliness.
func printListOptions(cmd *cobra.Command, jsonOutput bool) {
	if jsonOutput {
		schema := map[string]interface{}{
			"options": map[string]interface{}{
				"memory":            map[string]interface{}{"type": "int", "default": 4, "unit": "GB", "hint": "RAM allocation for the VM"},
				"disk":              map[string]interface{}{"type": "int", "default": 40, "unit": "GB", "hint": "VM disk size (advanced, not in wizard)"},
				"cpu":               map[string]interface{}{"type": "int", "default": 4, "hint": "CPU cores (advanced, not in wizard)"},
				"start_dir":         map[string]interface{}{"type": "path", "default": "~/code", "hint": "Directory to cd into on entry. Must be under a mounted path"},
				"color":             map[string]interface{}{"type": "hex", "default": "auto", "hint": "iTerm2 background color (6-char hex, no #)"},
				"stacks":            map[string]interface{}{"type": "list", "values": []string{"web", "cloud", "dotnet", "python", "go", "rust", "data", "ollama"}, "hint": "Provisioning bundles to install"},
				"gpg_signing":       map[string]interface{}{"type": "bool", "default": false, "hint": "Enable GPG commit signing in VM"},
				"claude_local":      map[string]interface{}{"type": "bool", "default": false, "hint": "Run Claude Code against local Ollama instead of Anthropic cloud (requires ollama stack)"},
				"dotnet_version":    map[string]interface{}{"type": "string", "default": "10", "hint": ".NET SDK major version"},
				"node_version":      map[string]interface{}{"type": "string", "default": "lts", "hint": "Node.js version (lts, 22, 20, latest)"},
				"python_version":    map[string]interface{}{"type": "string", "default": "latest", "hint": "Python version via pyenv"},
				"go_version":        map[string]interface{}{"type": "string", "default": "latest", "hint": "Go version (e.g., 1.24)"},
				"rust_version":      map[string]interface{}{"type": "string", "default": "stable", "hint": "Rust toolchain (stable, nightly, 1.83)"},
				"terraform_version": map[string]interface{}{"type": "string", "default": "latest", "hint": "Terraform version"},
			},
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		enc.Encode(schema)
		return
	}

	cmd.Println("Configurable profile options:")
	cmd.Println()
	cmd.Println("  --memory          int     VM memory in gigabytes (default 4)")
	cmd.Println("  --disk            int     VM disk size in gigabytes (default 40)")
	cmd.Println("  --cpu             int     Number of virtual CPUs (default 4)")
	cmd.Println("  --start-dir       string  Working directory when attaching (default ~/code)")
	cmd.Println("  --color           string  Terminal accent color, 6-char hex (auto-assigned if omitted)")
	cmd.Println("  --stack           string  Comma-separated stacks: web, cloud, dotnet, python, go, rust, data, ollama")
	cmd.Println("  --gpg-signing     bool    Enable GPG commit-signing (default false)")
	cmd.Println("  --claude-local    bool    Run Claude Code against local Ollama (default false)")
	cmd.Println("  --dotnet-version  string  Pin .NET SDK version")
	cmd.Println("  --node-version    string  Pin Node.js version")
	cmd.Println("  --python-version  string  Pin Python version")
	cmd.Println("  --go-version      string  Pin Go toolchain version")
	cmd.Println("  --rust-version    string  Pin Rust toolchain version")
	cmd.Println("  --terraform-version string Pin Terraform CLI version")
}

// printJSON serialises the created profile to JSON and writes it to the
// command's output stream.
func printJSON(cmd *cobra.Command, name string, p *config.Profile) error {
	result := createProfileResult{Name: name, Config: p}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// agentDataDir returns the host-side path for an agent profile's persistent data.
func agentDataDir(profile, agentType string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cloister", "agents", profile, agentType), nil
}
