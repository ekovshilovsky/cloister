package cmd

import (
	"fmt"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

var logsFollow bool

func init() {
	rootCmd.AddCommand(logsCmd)
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Stream logs in real-time")
}

// logsCmd tails or streams logs for a named profile. For Lume profiles, it
// queries the OpenClaw gateway daemon logs via SSH. For Colima profiles, it
// queries Docker container logs.
var logsCmd = &cobra.Command{
	Use:   "logs <profile>",
	Short: "View logs for a profile",
	Long: `Print the last 100 lines of logs for the named profile, or stream
logs in real-time with --follow.

For Lume/OpenClaw profiles: queries openclaw gateway logs.
For Colima profiles: queries Docker container logs.`,
	Args: cobra.ExactArgs(1),
	RunE: runLogs,
}

func runLogs(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	p, ok := cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}

	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	if !backend.IsRunning(name) {
		return fmt.Errorf("profile %q is not running", name)
	}

	if strings.EqualFold(p.Backend, "lume") {
		return lumeLogs(name, backend, logsFollow)
	}
	return colimaLogs(name, backend, logsFollow)
}

// lumeLogs queries OpenClaw gateway logs via SSH inside a Lume VM.
func lumeLogs(profile string, backend vm.Backend, follow bool) error {
	if follow {
		return backend.SSHInteractive(profile,
			`export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH" && openclaw gateway logs --follow`)
	}
	out, err := backend.SSHCommand(profile,
		`export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH" && openclaw gateway logs --tail 100`)
	if err != nil {
		return fmt.Errorf("reading OpenClaw logs: %w", err)
	}
	fmt.Print(out)
	return nil
}

// colimaLogs queries Docker container logs via SSH inside a Colima VM.
func colimaLogs(profile string, backend vm.Backend, follow bool) error {
	// Find the OpenClaw gateway container by naming convention.
	containerName := profile + "-gateway"
	if follow {
		return backend.SSHInteractive(profile,
			fmt.Sprintf("docker logs -f %s 2>&1", containerName))
	}
	out, err := backend.SSHCommand(profile,
		fmt.Sprintf("docker logs --tail 100 %s 2>&1", containerName))
	if err != nil {
		return fmt.Errorf("reading container logs: %w", err)
	}
	fmt.Print(out)
	return nil
}
