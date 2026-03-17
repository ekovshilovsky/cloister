package cmd

import "github.com/spf13/cobra"

// Version is set at build time via -ldflags.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:   "cloister",
	Short: "Isolated environments for multiple Claude Code accounts",
	Long: `cloister creates and manages isolated environments for running
multiple Claude Code accounts on a single Mac. Each profile gets its
own credentials, conversation history, and CLAUDE.md while sharing
your code workspace and SSH keys.`,
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
