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
		fmt.Print(agentRemovalAdvice())
		return nil
	},
}

// agentRemovalAdvice names the commands that took over the removed subcommand
// tree. Each one is registered on the root command, which is what
// TestAgentRemovalAdviceNamesOnlyRealCommands enforces.
func agentRemovalAdvice() string {
	return `The 'agent' command has been removed. The profile commands do its work:

  cloister <profile>                 Start a VM, and enter it when it has a session
  cloister exec <profile> <command>  Run a command inside a VM
  cloister logs <profile>            View a profile's logs
  cloister status                    Show every profile's state
  cloister stop <profile>            Stop a VM
`
}
