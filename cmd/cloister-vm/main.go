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

var doctorJSONFlag bool

var statusBriefFlag bool
var statusJSONFlag bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show VM environment status",
	Long: `Displays a status overview of the cloister VM environment, including
the active profile, Claude mode, tunnel health, and workspace path.

Use --brief for a compact one-line summary suitable for shell login banners.
Use --json for machine-readable structured output.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := vmcli.LoadConfig(vmcli.DefaultConfigPath())
		if err != nil {
			return err
		}

		// Use a reduced timeout in brief mode to avoid adding latency to shell
		// startup when tunnels are temporarily unavailable.
		timeoutMs := 500
		if statusBriefFlag {
			timeoutMs = 100
		}

		results := vmcli.CheckTunnels(cfg.Tunnels, timeoutMs)

		// Fetch Ollama model count only in full mode when the ollama tunnel is up,
		// since the HTTP query adds non-trivial latency unsuitable for login banners.
		modelCount := 0
		if !statusBriefFlag {
			for _, r := range results {
				if r.Name == "ollama" && r.Connected {
					models, fetchErr := vmcli.FetchOllamaModels()
					if fetchErr == nil {
						modelCount = len(models)
					}
					break
				}
			}
		}

		if statusJSONFlag {
			data := vmcli.BuildStatusData(cfg, results, modelCount)
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(data)
		}

		if statusBriefFlag {
			fmt.Print(vmcli.FormatStatusBrief(cfg, results))
			return nil
		}

		fmt.Print(vmcli.FormatStatus(cfg, results, modelCount))
		return nil
	},
}

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
	RunE: func(cmd *cobra.Command, args []string) error {
		models, err := vmcli.FetchOllamaModels()
		if err != nil {
			return err
		}

		if modelsJSONFlag {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(models)
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
		return nil
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
		fmt.Println("Run: claude --model qwen2.5-coder:7b")
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

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Run diagnostic checks on the VM environment",
	Long: `Runs a series of diagnostic checks to verify that the VM environment
is correctly configured and all services are operational. Each check runs
independently so that one failure does not prevent subsequent checks.

Use --json for machine-readable structured output suitable for automation.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Attempt to load config for checks that need it, but proceed with
		// nil if the config file is missing or malformed since the doctor
		// command should still report on all other checks.
		cfg, _ := vmcli.LoadConfig(vmcli.DefaultConfigPath())

		results := vmcli.RunDoctor(cfg)

		if doctorJSONFlag {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(results)
		}

		for _, r := range results {
			fmt.Println(r.String())
		}
		fmt.Println()
		fmt.Println(vmcli.FormatDoctorSummary(results))
		return nil
	},
}

func init() {
	statusCmd.Flags().BoolVar(&statusBriefFlag, "brief", false, "Output a compact one-line summary (reduced probe timeout for login banners)")
	statusCmd.Flags().BoolVar(&statusJSONFlag, "json", false, "Output status as a structured JSON object")
	modelsCmd.Flags().BoolVar(&modelsJSONFlag, "json", false, "Output models as JSON")
	tunnelsCmd.Flags().BoolVar(&tunnelsJSONFlag, "json", false, "Output tunnel results as JSON")
	claudeLocalCmd.Flags().BoolVar(&claudeLocalEvalFlag, "eval", false, "Output export statements for eval instead of human-readable output")
	claudeCloudCmd.Flags().BoolVar(&claudeCloudEvalFlag, "eval", false, "Output unset statements for eval instead of human-readable output")
	doctorCmd.Flags().BoolVar(&doctorJSONFlag, "json", false, "Output diagnostic results as JSON")
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(modelsCmd)
	rootCmd.AddCommand(tunnelsCmd)
	rootCmd.AddCommand(claudeLocalCmd)
	rootCmd.AddCommand(claudeCloudCmd)
	rootCmd.AddCommand(doctorCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
