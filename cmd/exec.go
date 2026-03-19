package cmd

import (
	"fmt"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(execCmd)
}

var execCmd = &cobra.Command{
	Use:   "exec <profile> <command...>",
	Short: "Run a command inside a profile's VM without entering it",
	Long: `Execute a shell command inside the named profile's VM and print
the output. The VM must already be running. This is useful for one-off
administration tasks, installing tools, or scripting VM operations
without opening an interactive session.

Examples:
  cloister exec work claude --version
  cloister exec dev "curl -fsSL https://example.com/install.sh | bash"
  cloister exec ci-agent ollama list`,
	Args: cobra.MinimumNArgs(2),
	RunE: runExec,
}

// runExec executes a command inside the named profile's VM and prints the
// combined stdout/stderr output. The VM must be running; starting it
// automatically is intentionally avoided so that exec remains a lightweight,
// non-destructive operation.
func runExec(cmd *cobra.Command, args []string) error {
	profileName := args[0]
	command := strings.Join(args[1:], " ")

	cfgPath, err := config.ConfigPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	if _, ok := cfg.Profiles[profileName]; !ok {
		return fmt.Errorf("profile %q not found", profileName)
	}

	if !vm.IsRunning(profileName) {
		return fmt.Errorf("profile %q is not running. Start it with: cloister %s", profileName, profileName)
	}

	output, err := vm.SSHCommand(profileName, command)
	if output != "" {
		fmt.Print(output)
	}
	if err != nil {
		return fmt.Errorf("command failed: %w", err)
	}
	return nil
}
