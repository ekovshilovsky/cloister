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
