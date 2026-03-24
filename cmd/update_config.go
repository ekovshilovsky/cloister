package cmd

import (
	"fmt"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/provision"
	"github.com/ekovshilovsky/cloister/internal/tunnel"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(updateConfigCmd)
	f := updateConfigCmd.Flags()
	f.Bool("claude-local", false, "Enable local Claude Code via Ollama")
	f.Bool("claude-cloud", false, "Disable local Claude Code (use Anthropic cloud)")
}

var updateConfigCmd = &cobra.Command{
	Use:   "update-config <profile>",
	Short: "Update a profile's configuration without rebuilding",
	Long: `Update configuration flags on a running profile. Changes that affect
the bashrc (like --claude-local) are applied immediately by redeploying
the managed bashrc into the VM.

Examples:
  cloister update-config work --claude-local        Enable offline Claude Code
  cloister update-config work --claude-cloud     Switch back to Anthropic cloud`,
	Args: cobra.ExactArgs(1),
	RunE: runUpdateConfig,
}

// runUpdateConfig modifies a profile's configuration and redeploys affected
// files without requiring a full rebuild.
func runUpdateConfig(cmd *cobra.Command, args []string) error {
	profileName := args[0]

	// Validate mutually exclusive flags before doing any I/O.
	claudeLocalSet := cmd.Flags().Changed("claude-local")
	claudeCloudSet := cmd.Flags().Changed("claude-cloud")
	if claudeLocalSet && claudeCloudSet {
		return fmt.Errorf("--claude-local and --claude-cloud are mutually exclusive")
	}

	cfgPath, err := config.ConfigPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	p, ok := cfg.Profiles[profileName]
	if !ok {
		return fmt.Errorf("profile %q not found", profileName)
	}

	changed := false

	if claudeLocalSet {
		// Validate that the ollama stack is present — local Claude Code
		// requires the Ollama tunnel and CLI inside the VM.
		if !p.HasStack("ollama") {
			return fmt.Errorf("--claude-local requires the ollama stack. Add it first: cloister add-stack %s ollama", profileName)
		}

		p.ClaudeLocal = true
		changed = true
		fmt.Println("Claude Code local mode: enabled")
		fmt.Println("  Claude Code will use the host's Ollama via Anthropic API compatibility.")
		fmt.Println("  Run: claude --model qwen2.5-coder:7b")
	}

	if claudeCloudSet {
		p.ClaudeLocal = false
		changed = true
		fmt.Println("Claude Code local mode: disabled")
		fmt.Println("  Claude Code will use Anthropic's cloud API.")
	}

	if !changed {
		return fmt.Errorf("no configuration changes specified. Use --claude-local or --claude-cloud")
	}

	// Persist the updated configuration.
	if err := config.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	// Resolve the backend to check whether the VM is currently running so
	// that configuration changes can be applied immediately.
	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	// If the VM is running, redeploy the bashrc so changes take effect
	// on the next shell login without requiring a rebuild.
	if backend.IsRunning(profileName) {
		fmt.Println("Redeploying bashrc...")
		if err := provision.DeployBashrc(profileName, p); err != nil {
			return fmt.Errorf("redeploying bashrc: %w", err)
		}
		if err := provision.DeployVMConfig(profileName, p, tunnel.BuiltinTunnelDefs(), provision.ResolveStartDir(p.StartDir)); err != nil {
			fmt.Printf("Warning: deploying VM config: %v\n", err)
		}
		fmt.Println("Changes applied. Open a new shell in the VM for them to take effect.")
	} else {
		fmt.Println("Config saved. Changes will apply when the VM starts.")
	}

	return nil
}
