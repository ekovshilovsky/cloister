package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(agentCmd)
}

// agentCmd is a deprecation shim that directs users to the unified profile
// commands. The agent subcommand tree was removed when Lume profiles eliminated
// the need for a separate container lifecycle inside the VM.
var agentCmd = &cobra.Command{
	Use:    "agent",
	Short:  "Deprecated — use profile commands directly",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("The 'agent' command has been removed.")
		fmt.Println()
		fmt.Println("Use these commands instead:")
		fmt.Println("  cloister start <profile>    Start a VM")
		fmt.Println("  cloister stop <profile>     Stop a VM")
		fmt.Println("  cloister status             Show all profile status")
		fmt.Println("  cloister logs <profile>     View logs")
		fmt.Println("  cloister forward <profile> <port>  Forward a port")
		return nil
	},
}
