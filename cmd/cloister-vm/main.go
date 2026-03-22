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

func init() {
	modelsCmd.Flags().BoolVar(&modelsJSONFlag, "json", false, "Output models as JSON")
	tunnelsCmd.Flags().BoolVar(&tunnelsJSONFlag, "json", false, "Output tunnel results as JSON")
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(modelsCmd)
	rootCmd.AddCommand(tunnelsCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
