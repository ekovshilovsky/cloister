package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(execCmd)
	// SilenceUsage stops cobra from printing the full --help block whenever
	// RunE returns non-nil, which used to fire on every inner-command
	// non-zero exit. SilenceErrors stops cobra from also printing
	// "Error: command failed: <wrapped>" on top of the output the inner
	// command already wrote. Together they let `cloister exec` behave like
	// a transparent shell pipe: the user sees the inner command's stdout
	// and stderr, and the cloister wrapper sets a non-zero process exit
	// code without adding any of its own diagnostic chatter.
	execCmd.SilenceUsage = true
	execCmd.SilenceErrors = true
}

var execCmd = &cobra.Command{
	Use:   "exec <profile> <command...>",
	Short: "Run a command inside a profile's VM without entering it",
	Long: `Execute a shell command inside the named profile's VM and print
the output. The VM must already be running. This is useful for one-off
administration tasks, installing tools, or scripting VM operations
without opening an interactive session.

The inner command's stdout and stderr are streamed through transparently,
and its non-zero exit is propagated as cloister's own non-zero exit so
'cloister exec <profile> <cmd>' is drop-in interchangeable with running
<cmd> inside the VM for shell pipelines and CI scripts. cloister does
not add its own "command failed" wrapping around an inner exit, because
the inner output above it has already conveyed the failure to the user.

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
//
// Exit-code handling: when the inner command exits non-zero, the backend
// returns an error wrapping the shell-level exit. We surface that as a
// silent non-zero process exit (cobra prints nothing thanks to
// SilenceErrors/SilenceUsage in init) so the caller sees only the inner
// command's own output, exactly as if they had run the command directly
// inside the VM. Backend-side errors (profile not found, VM not running,
// SSH transport failure) are still surfaced with a descriptive message
// because there is no inner-command output to convey them.
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

	p, ok := cfg.Profiles[profileName]
	if !ok {
		return fmt.Errorf("profile %q not found", profileName)
	}

	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	if !backend.IsRunning(profileName) {
		return fmt.Errorf("profile %q is not running. Start it with: cloister %s", profileName, profileName)
	}

	output, err := backend.SSHCommand(profileName, command)
	if output != "" {
		fmt.Print(output)
	}
	if err != nil {
		// Inner-command exit: return a sentinel error so cobra's silenced
		// error handling exits non-zero without printing anything on top
		// of the output the inner command already wrote. The user sees a
		// transparent passthrough.
		return errSilentExit
	}
	return nil
}

// errSilentExit is returned from runExec when the inner command exited
// non-zero. cobra's SilenceErrors flag prevents it from being printed; its
// only effect is propagating the non-zero exit code to the parent process.
// Using a distinct sentinel (rather than wrapping the backend error) makes
// the intent explicit at the call site and keeps tests deterministic.
var errSilentExit = errors.New("exec: inner command exited non-zero")
