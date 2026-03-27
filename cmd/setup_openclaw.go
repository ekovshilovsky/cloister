package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/setup"
	vmlume "github.com/ekovshilovsky/cloister/internal/vm/lume"
	"github.com/spf13/cobra"
)

// setupOpenclawFlags holds flag values for the setup openclaw subcommand.
type setupOpenclawFlags struct {
	yes         bool
	listOptions bool
	jsonOutput  bool

	skipTelegram    bool
	skipWhatsApp    bool
	skipGoogleOAuth bool
	skipPairing     bool

	telegramToken      string
	telegramUserID     string
	whatsAppNumber     string
	defaultProvider    string
	anthropicAPIKey    string
	ollamaModel        string
	openaiAPIKey       string
	googlePlacesAPIKey string
	googleClientSecret string
}

var socf setupOpenclawFlags

func init() {
	setupCmd.AddCommand(setupOpenclawCmd)

	f := setupOpenclawCmd.Flags()
	f.BoolVarP(&socf.yes, "yes", "y", false, "Auto-create VM if it doesn't exist")
	f.BoolVar(&socf.listOptions, "list-options", false, "Output all configurable options as JSON and exit")
	f.BoolVar(&socf.jsonOutput, "json", false, "Emit output as JSON")

	f.BoolVar(&socf.skipTelegram, "skip-telegram", false, "Skip Telegram channel setup")
	f.BoolVar(&socf.skipWhatsApp, "skip-whatsapp", false, "Skip WhatsApp channel setup")
	f.BoolVar(&socf.skipGoogleOAuth, "skip-google-oauth", false, "Skip Google OAuth setup")
	f.BoolVar(&socf.skipPairing, "skip-pairing", false, "Skip device pairing")

	f.StringVar(&socf.telegramToken, "telegram-token", "", "Telegram bot token")
	f.StringVar(&socf.telegramUserID, "telegram-user-id", "", "Telegram user ID for bot locking")
	f.StringVar(&socf.whatsAppNumber, "whatsapp-number", "", "WhatsApp phone number for trusted sender")
	f.StringVar(&socf.defaultProvider, "default-provider", "", "Default AI provider (ollama or anthropic)")
	f.StringVar(&socf.anthropicAPIKey, "anthropic-api-key", "", "Anthropic API key")
	f.StringVar(&socf.ollamaModel, "ollama-model", "", "Ollama model to use as primary")
	f.StringVar(&socf.openaiAPIKey, "openai-api-key", "", "OpenAI API key")
	f.StringVar(&socf.googlePlacesAPIKey, "google-places-api-key", "", "Google Places API key")
	f.StringVar(&socf.googleClientSecret, "google-client-secret", "", "Path to Google OAuth client_secret.json")
}

var setupOpenclawCmd = &cobra.Command{
	Use:   "openclaw [profile]",
	Short: "Guided setup for an OpenClaw VM",
	Long: `Configure an OpenClaw Lume VM with credentials, channels, providers,
Google OAuth, and device pairing. Runs as a linear walkthrough on first
invocation, then presents a menu on subsequent runs.

Without a profile argument in interactive mode, lists available Lume VMs
to select from. In non-interactive mode, the profile name is required.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSetupOpenclaw,
}

func runSetupOpenclaw(cmd *cobra.Command, args []string) error {
	// Determine interactivity: interactive when no profile arg and no value flags.
	hasValueFlags := socf.telegramToken != "" || socf.defaultProvider != "" ||
		socf.anthropicAPIKey != "" || socf.ollamaModel != ""
	interactive := isInteractive() && len(args) == 0 && !socf.listOptions && !hasValueFlags

	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Resolve profile name via interactive selection or positional argument.
	var profileName string
	if len(args) == 1 {
		profileName = args[0]
	} else if interactive {
		name, err := selectLumeVM(cfg)
		if err != nil {
			return err
		}
		profileName = name
	} else {
		return fmt.Errorf("profile name is required in non-interactive mode")
	}

	// Verify the profile exists or create with -y.
	p, ok := cfg.Profiles[profileName]
	if !ok {
		if !socf.yes {
			return fmt.Errorf("profile %q not found — use -y to create it automatically", profileName)
		}
		fmt.Printf("Creating OpenClaw VM %q...\n", profileName)
		// Delegate to the create command's RunE with --openclaw --defaults.
		cf.openclaw = true
		cf.defaults = true
		if err := createCmd.RunE(createCmd, []string{profileName}); err != nil {
			return fmt.Errorf("creating VM: %w", err)
		}
		// Reload config after creation.
		cfg, err = config.Load(cfgPath)
		if err != nil {
			return fmt.Errorf("reloading config after VM creation: %w", err)
		}
		p = cfg.Profiles[profileName]
		if p == nil {
			return fmt.Errorf("profile %q not found after creation", profileName)
		}
	}

	if !strings.EqualFold(p.Backend, "lume") {
		return fmt.Errorf("profile %q uses backend %q — setup openclaw requires a Lume profile", profileName, p.Backend)
	}

	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	// Ensure the VM is running before starting setup.
	if !backend.IsRunning(profileName) {
		fmt.Printf("Starting VM %q...\n", profileName)
		if err := backend.Start(profileName, p.CPU, p.Memory, p.Disk, nil, false); err != nil {
			return fmt.Errorf("starting VM: %w", err)
		}
	}

	// Load setup state and progress.
	statePath, err := setup.StatePath(profileName)
	if err != nil {
		return fmt.Errorf("resolving setup state path: %w", err)
	}
	state, err := setup.LoadState(statePath)
	if err != nil {
		return fmt.Errorf("loading setup state: %w", err)
	}

	progressPath, err := setup.ProgressPath(profileName)
	if err != nil {
		return fmt.Errorf("resolving progress path: %w", err)
	}
	progress, err := setup.LoadProgress(progressPath)
	if err != nil {
		return fmt.Errorf("loading progress: %w", err)
	}

	// Handle --list-options: output current state as JSON and exit.
	if socf.listOptions {
		return printSetupOptions(profileName, state)
	}

	// Detect or reuse credential store.
	var creds setup.CredentialStore
	if state.CredentialStore == "op" {
		creds = setup.NewOpStore()
	} else if state.CredentialStore == "local" {
		creds, err = setup.NewDefaultLocalStore()
		if err != nil {
			return fmt.Errorf("initializing local credential store: %w", err)
		}
	}
	// If creds is nil, the credentials section will handle detection and prompting.

	ctx := &setup.SetupContext{
		Profile:      profileName,
		Backend:      backend,
		State:        state,
		Progress:     progress,
		Creds:        creds,
		Interactive:  interactive,
		StatePath:    statePath,
		ProgressPath: progressPath,
	}

	sections := setup.AllSections()

	if setup.IsFirstRun(state) || progress.FailedStep != nil {
		// Linear walkthrough or resume from failure.
		for _, section := range sections {
			fmt.Printf("\n──── %s ────────────────────────────\n", section.Description)
			if err := section.Run(ctx); err != nil {
				return fmt.Errorf("%s: %w", section.Name, err)
			}
		}
	} else if interactive {
		// Menu mode for re-runs.
		return showSetupMenu(ctx, sections)
	} else {
		// Non-interactive re-run: run all sections (they check their own state).
		for _, section := range sections {
			if err := section.Run(ctx); err != nil {
				return fmt.Errorf("%s: %w", section.Name, err)
			}
		}
	}

	// Clear progress on successful completion.
	if err := setup.ClearProgress(progressPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not clear progress file: %v\n", err)
	}

	fmt.Printf("\n  Setup complete for %s\n", profileName)
	fmt.Printf("\n  Status:   cloister status %s\n", profileName)
	fmt.Printf("  Logs:     cloister logs %s\n", profileName)
	fmt.Printf("  Re-run:   cloister setup openclaw %s\n", profileName)
	return nil
}

// selectLumeVM lists Lume-backend VMs and lets the user pick one or create new.
func selectLumeVM(cfg *config.Config) (string, error) {
	lumeBackend := &vmlume.Backend{}
	vms, err := lumeBackend.List(false)
	if err != nil {
		vms = nil
	}

	type vmEntry struct {
		name   string
		status string
	}
	var entries []vmEntry
	for _, v := range vms {
		profileName := lumeBackend.ProfileFromVMName(v.Name)
		if profileName == "" {
			continue
		}
		if p, ok := cfg.Profiles[profileName]; ok && strings.EqualFold(p.Backend, "lume") {
			entries = append(entries, vmEntry{name: profileName, status: strings.ToLower(v.Status)})
		}
	}

	fmt.Println("\nSelect an OpenClaw VM:")
	for i, e := range entries {
		fmt.Printf("  [%d] %s (%s)\n", i+1, e.name, e.status)
	}
	fmt.Printf("  [%d] Create new VM\n", len(entries)+1)
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("> ")
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading selection: %w", err)
	}

	choice, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || choice < 1 || choice > len(entries)+1 {
		return "", fmt.Errorf("invalid selection")
	}

	if choice <= len(entries) {
		return entries[choice-1].name, nil
	}

	// Create new VM — prompt for name.
	fmt.Print("Enter VM name: ")
	nameLine, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading VM name: %w", err)
	}
	name := strings.TrimSpace(nameLine)
	if name == "" {
		return "", fmt.Errorf("VM name cannot be empty")
	}
	return name, nil
}

// showSetupMenu displays the re-run menu showing configured vs unconfigured sections.
func showSetupMenu(ctx *setup.SetupContext, sections []setup.Section) error {
	fmt.Printf("\n%s — Setup Menu\n", ctx.Profile)
	for i, s := range sections {
		marker := "✗"
		if s.IsConfigured(ctx.State) {
			marker = "✓"
		}
		fmt.Printf("  %s [%d] %s\n", marker, i+1, s.Description)
	}
	fmt.Printf("  [%d] Run full setup again\n", len(sections)+1)
	fmt.Println("  [q] Quit")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("> ")
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading selection: %w", err)
	}
	input := strings.TrimSpace(line)

	if strings.EqualFold(input, "q") {
		return nil
	}

	choice, err := strconv.Atoi(input)
	if err != nil || choice < 1 || choice > len(sections)+1 {
		return fmt.Errorf("invalid selection")
	}

	if choice == len(sections)+1 {
		for _, section := range sections {
			if err := section.Run(ctx); err != nil {
				return fmt.Errorf("%s: %w", section.Name, err)
			}
		}
		return nil
	}

	return sections[choice-1].Run(ctx)
}

// printSetupOptions outputs the current setup state as JSON for AI-friendly discovery.
func printSetupOptions(profile string, state *setup.SetupState) error {
	output := map[string]interface{}{
		"profile":          profile,
		"credential_store": state.CredentialStore,
		"sections": map[string]interface{}{
			"credentials": map[string]bool{
				"configured": state.Credentials.KeychainPassword && state.Credentials.VMLumeUser,
			},
			"channels": map[string]interface{}{
				"telegram": map[string]interface{}{
					"configured":   state.Channels.Telegram.Configured,
					"bot_username": state.Channels.Telegram.BotUsername,
				},
				"whatsapp": map[string]interface{}{
					"configured": state.Channels.WhatsApp.Configured,
					"mode":       state.Channels.WhatsApp.Mode,
				},
			},
			"providers": map[string]interface{}{
				"ollama":           map[string]bool{"configured": state.Providers.Ollama.Configured},
				"anthropic":        map[string]bool{"configured": state.Providers.Anthropic.Configured},
				"default_provider": state.Providers.DefaultProvider,
			},
			"google_oauth": map[string]interface{}{
				"configured": len(state.OAuth.GoogleServices) > 0,
				"services":   state.OAuth.GoogleServices,
			},
			"pairing": map[string]bool{
				"configured": state.Pairing.DevicesApproved,
			},
		},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}
