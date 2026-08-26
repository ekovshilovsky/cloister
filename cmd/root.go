package cmd

import (
	"fmt"
	"os/exec"

	"cloister.io/internal/vm"
	vmcolima "cloister.io/internal/vm/colima"
	vmlume "cloister.io/internal/vm/lume"
	"github.com/spf13/cobra"
)

// resolveBackend returns the vm.Backend implementation for the given backend
// name. Empty string defaults to "colima" for backward compatibility.
func resolveBackend(backendName string) (vm.Backend, error) {
	name, err := vm.ResolveBackendName(backendName)
	if err != nil {
		return nil, err
	}
	switch name {
	case "colima":
		return &vmcolima.Backend{}, nil
	case "lume":
		// Verify the lume CLI is installed before returning the backend so
		// callers receive an actionable installation message rather than a
		// cryptic "command not found" error later in the lifecycle.
		if _, err := exec.LookPath("lume"); err != nil {
			return nil, fmt.Errorf("lume CLI not found. Install: curl -fsSL https://raw.githubusercontent.com/trycua/lume/main/scripts/install.sh | bash")
		}
		return &vmlume.Backend{}, nil
	default:
		return nil, fmt.Errorf("unknown backend: %s", name)
	}
}

// Version is set at build time via -ldflags.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:     "cloister [profile]",
	Short:   "Isolated VM environments for AI coding agents and multi-account separation",
	Version: Version,
	Long: `cloister creates and manages isolated macOS VM environments for running
AI coding agents securely, separating multiple Claude Code accounts, and
sandboxing autonomous tools like OpenClaw. Each profile gets its own
credentials and session history while sharing your code workspace.

To enter a profile's VM, run:

  cloister <profile>      Start tunnels and open an interactive SSH session
  cloister status         Show all profiles and their current state
  cloister create <name>  Create a new profile`,
	// Accept any argument so that a bare profile name (e.g. "cloister work")
	// reaches RunE rather than being rejected by Cobra's unknown-command check.
	Args: cobra.ArbitraryArgs,
	// RunE treats a single positional argument as a profile name, delegating
	// directly to enterProfile. Any other invocation without a recognized
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
		// Write to the command's stdout writer explicitly. Cobra's cmd.Printf
		// falls back to stderr when no output writer is set, which leaves
		// stdout-only consumers such as the Homebrew formula test with no
		// output at all.
		fmt.Fprintf(cmd.OutOrStdout(), "cloister %s\n", Version)
	},
}
