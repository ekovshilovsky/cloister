package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/profile"
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
		printListOptions(cmd)
		return nil
	}

	// Validate the profile name before doing any I/O.
	if err := profile.ValidateName(name); err != nil {
		return err
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
		cmd.Flags().Changed("disk") ||
		cmd.Flags().Changed("cpu") ||
		cmd.Flags().Changed("dotnet-version") ||
		cmd.Flags().Changed("node-version") ||
		cmd.Flags().Changed("python-version") ||
		cmd.Flags().Changed("go-version") ||
		cmd.Flags().Changed("rust-version") ||
		cmd.Flags().Changed("terraform-version")

	p := &config.Profile{}

	if cf.defaults || anyFlagSet {
		// Non-interactive path: apply defaults then overlay any explicit flags.
		p.ApplyDefaults()
		applyFlagsToProfile(p, cmd)
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

	// Persist the new profile.
	cfg.Profiles[name] = p
	if err := config.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	// Emit output.
	if cf.jsonOutput {
		return printJSON(cmd, name, p)
	}

	cmd.Printf("Profile %q created. Enter with: cloister %s\n", name, name)
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
// When the user accepts defaults at the top-level prompt the function applies
// package defaults and returns immediately without further prompting.
func runInteractiveWizard(p *config.Profile, cfg *config.Config) error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Use defaults? (4GB RAM, ~/Code, auto color) [Y/n]: ")
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

	// Step through each field individually.
	p.ApplyDefaults() // start from defaults so unmodified fields are not zero

	memory, err := promptInt(reader,
		fmt.Sprintf("Memory in GB [%d]: ", config.DefaultMemory),
		config.DefaultMemory)
	if err != nil {
		return err
	}
	p.Memory = memory

	startDir, err := promptString(reader,
		fmt.Sprintf("Start directory [%s]: ", config.DefaultStartDir),
		config.DefaultStartDir)
	if err != nil {
		return err
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

// printListOptions writes the list of all configurable profile fields and their
// permitted values to the command's output stream.
func printListOptions(cmd *cobra.Command) {
	cmd.Println("Configurable profile options:")
	cmd.Println()
	cmd.Println("  --memory          int     VM memory in gigabytes (default 4)")
	cmd.Println("  --disk            int     VM disk size in gigabytes (default 40)")
	cmd.Println("  --cpu             int     Number of virtual CPUs (default 4)")
	cmd.Println("  --start-dir       string  Working directory when attaching (default ~/Code)")
	cmd.Println("  --color           string  Terminal accent color, 6-char hex (auto-assigned if omitted)")
	cmd.Println("  --stack           string  Comma-separated stacks: web, cloud, dotnet, python, go, rust, data")
	cmd.Println("  --gpg-signing     bool    Enable GPG commit-signing (default false)")
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
