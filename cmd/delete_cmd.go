package cmd

import (
	"fmt"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/tunnel"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(deleteCmd)
}

var deleteCmd = &cobra.Command{
	Use:   "delete <profile>",
	Short: "Delete a cloister profile and its VM",
	Long: `Permanently destroy the VM and remove the named profile from the
cloister configuration.

All isolated data stored inside the VM is lost when it is deleted. The
host-side directories mounted into the VM (e.g. ~/Code) are not affected.`,
	Args: cobra.ExactArgs(1),
	RunE: runDelete,
}

// runDelete is the handler for the delete subcommand.
func runDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if _, ok := cfg.Profiles[name]; !ok {
		return fmt.Errorf("profile %q not found", name)
	}

	fmt.Printf("Deleting %q (this destroys all isolated data)...\n", name)

	// Attempt to destroy the Colima VM. Errors are intentionally ignored here
	// because the VM may never have been started (i.e. it does not exist in
	// Colima's registry yet), and in that case we still want to remove the
	// profile entry from the configuration.
	tunnel.StopAll(name)
	_ = vm.Delete(name, false)

	delete(cfg.Profiles, name)

	if err := config.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("saving config after delete: %w", err)
	}

	fmt.Printf("Profile %q deleted\n", name)
	return nil
}
