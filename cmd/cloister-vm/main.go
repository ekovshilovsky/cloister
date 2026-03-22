// cmd/cloister-vm/main.go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ekovshilovsky/cloister/internal/vmcli"
)

// Version is set at build time via -ldflags.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:   "cloister-vm",
	Short: "Toolkit for cloister VM environments",
	Long: `cloister-vm provides status, diagnostics, and configuration tools
for use inside cloister-managed virtual machines.

Run 'cloister-vm status' for a quick overview of your VM environment.`,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print cloister-vm version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("cloister-vm %s\n", Version)
	},
}

var modelsJSONFlag bool

var tunnelsJSONFlag bool

var claudeLocalEvalFlag bool

var claudeCloudEvalFlag bool

var tunnelsCmd = &cobra.Command{
	Use:   "tunnels",
	Short: "Check the status of host service tunnels",
	Long: `Probes each tunnel defined in the VM config and reports whether each
service port is reachable. For well-known tunnels (op-forward, ollama), richer
health details are included when the tunnel is connected.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := vmcli.LoadConfig(vmcli.DefaultConfigPath())
		if err != nil {
			return err
		}

		results := vmcli.CheckTunnels(cfg.Tunnels, 500)

		if tunnelsJSONFlag {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(results)
		}

		for _, r := range results {
			fmt.Println(r.String())
		}
		return nil
	},
}

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List Ollama models available on the host GPU",
	Long: `Queries the Ollama API tunneled from the macOS host and displays
all installed models with their sizes and last-modified timestamps.`,
	Run: func(cmd *cobra.Command, args []string) {
		models, err := vmcli.FetchOllamaModels()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		if modelsJSONFlag {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(models); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}

		// Render a tab-aligned table for human-readable output.
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSIZE\tMODIFIED")
		for _, m := range models {
			fmt.Fprintf(w, "%s\t%s\t%s\n",
				m.Name,
				vmcli.FormatModelSize(m.Size),
				m.ModifiedAt.Format("2006-01-02"),
			)
		}
		w.Flush()

		fmt.Println("\nOllama server: host (Metal GPU via tunnel)")
	},
}

var claudeLocalCmd = &cobra.Command{
	Use:   "claude-local",
	Short: "Switch Claude Code to use the local Ollama server",
	Long: `Writes ~/.cloister-vm/claude-mode.env with shell exports that redirect
Claude Code from the Anthropic cloud API to the Ollama server tunneled from
the macOS host. Source the file (or use --eval) to apply changes in your shell.

Shell integration (applies immediately without a new login):
  eval $(cloister-vm claude-local --eval)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if claudeLocalEvalFlag {
			// Output only the export statements so the caller can eval them directly.
			fmt.Print(vmcli.ClaudeLocalEvalOutput())
			return nil
		}

		envPath := vmcli.DefaultClaudeEnvPath()
		if err := vmcli.WriteClaudeLocalEnv(envPath); err != nil {
			return err
		}

		fmt.Println("Claude Code mode: local")
		fmt.Printf("Env file written: %s\n", envPath)
		fmt.Printf("To apply in your current shell: source %s\n", envPath)
		fmt.Println("Or use shell integration:      eval $(cloister-vm claude-local --eval)")
		fmt.Println("Suggested model:               claude code -m claude-sonnet-4-5")
		return nil
	},
}

var claudeCloudCmd = &cobra.Command{
	Use:   "claude-cloud",
	Short: "Switch Claude Code back to the Anthropic cloud API",
	Long: `Removes ~/.cloister-vm/claude-mode.env and emits unset statements to
restore the default Anthropic cloud API configuration for Claude Code.

Shell integration (applies immediately without a new login):
  eval $(cloister-vm claude-cloud --eval)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if claudeCloudEvalFlag {
			// Output only the unset statements so the caller can eval them directly.
			fmt.Print(vmcli.ClaudeCloudEvalOutput())
			return nil
		}

		envPath := vmcli.DefaultClaudeEnvPath()
		if err := vmcli.RemoveClaudeEnv(envPath); err != nil {
			return err
		}

		fmt.Println("Claude Code mode: cloud")
		fmt.Printf("Env file removed: %s\n", envPath)
		fmt.Printf("To unset in your current shell: eval $(cloister-vm claude-cloud --eval)\n")
		return nil
	},
}

func init() {
	modelsCmd.Flags().BoolVar(&modelsJSONFlag, "json", false, "Output models as JSON")
	tunnelsCmd.Flags().BoolVar(&tunnelsJSONFlag, "json", false, "Output tunnel results as JSON")
	claudeLocalCmd.Flags().BoolVar(&claudeLocalEvalFlag, "eval", false, "Output export statements for eval instead of human-readable output")
	claudeCloudCmd.Flags().BoolVar(&claudeCloudEvalFlag, "eval", false, "Output unset statements for eval instead of human-readable output")
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(modelsCmd)
	rootCmd.AddCommand(tunnelsCmd)
	rootCmd.AddCommand(claudeLocalCmd)
	rootCmd.AddCommand(claudeCloudCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
