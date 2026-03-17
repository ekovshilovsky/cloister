package cmd

import "github.com/spf13/cobra"

// Version is set at build time via -ldflags.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:   "cloister",
	Short: "Isolated VM environments for AI coding agents and multi-account separation",
	Long: `cloister creates and manages isolated macOS VM environments for running
AI coding agents securely, separating multiple Claude Code accounts, and
sandboxing autonomous tools like OpenClaw. Each profile gets its own
credentials and session history while sharing your code workspace.`,
	// Accept any argument so that a bare profile name (e.g. "cloister work")
	// reaches RunE rather than being rejected by Cobra's unknown-command check.
	Args: cobra.ArbitraryArgs,
	// RunE treats a single positional argument as a profile name, delegating
	// directly to enterProfile. Any other invocation without a recognised
	// subcommand falls back to displaying the help text.
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return enterProfile(args[0])
		}
		return cmd.Help()
	},
}

// Execute is the entry point called by main; it runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print cloister version",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Printf("cloister %s\n", Version)
	},
}
