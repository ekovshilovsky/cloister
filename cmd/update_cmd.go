package cmd

import (
	"fmt"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(updateCmd)
}

var updateCmd = &cobra.Command{
	Use:   "update <profile|all>",
	Short: "Update Claude Code and system packages in running VMs",
	Long: `Upgrade Claude Code to the latest version and apply pending system package
updates inside the VM for the named profile.

Pass "all" to update every currently running profile VM in sequence. Stopped
VMs are skipped silently — start them first with "cloister start <profile>"
before running an update.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.EqualFold(args[0], "all") {
			return updateAll()
		}
		return updateProfile(args[0])
	},
}

// updateProfile upgrades Claude Code and applies system package updates in the
// VM for the named profile. The profile's VM must be running before the update
// is attempted; an actionable error is returned when it is not.
func updateProfile(name string) error {
	if !vm.IsRunning(name) {
		return fmt.Errorf("profile %q is not running. Start it first with: cloister start %s", name, name)
	}

	fmt.Printf("Updating %q...\n", name)

	// Upgrade Claude Code via the official installer, falling back to the npm
	// global package manager when the installer is unavailable.
	if out, err := vm.SSHCommand(name, "claude install latest 2>&1 || npm update -g @anthropic-ai/claude-code 2>&1"); err != nil {
		return fmt.Errorf("updating Claude Code in %q: %w\n%s", name, err, out)
	}

	// Apply pending system package updates. The -qq flag suppresses informational
	// output so only warnings and errors surface to the operator.
	if out, err := vm.SSHCommand(name, "sudo apt-get update -qq && sudo apt-get upgrade -y -qq"); err != nil {
		return fmt.Errorf("updating system packages in %q: %w\n%s", name, err, out)
	}

	fmt.Printf("✓ %s updated\n", name)
	return nil
}

// updateAll loads the configuration and calls updateProfile for every profile
// whose VM is currently running. Profiles with stopped VMs are skipped
// silently so that a partially-stopped fleet does not block the operation.
func updateAll() error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	var lastErr error
	for name := range cfg.Profiles {
		// Skip profiles whose VMs are not currently running to avoid blocking
		// the update loop on profiles that have not been started.
		if !vm.IsRunning(name) {
			continue
		}

		if err := updateProfile(name); err != nil {
			fmt.Printf("error updating %q: %v\n", name, err)
			lastErr = err
		}
	}

	return lastErr
}
