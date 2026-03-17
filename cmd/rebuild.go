package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/backup"
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

// rebuildFlags holds flag state for the rebuild subcommand.
type rebuildFlags struct {
	yes bool
}

var rbf rebuildFlags

func init() {
	rootCmd.AddCommand(rebuildCmd)
	rebuildCmd.Flags().BoolVarP(&rbf.yes, "yes", "y", false, "Skip the confirmation prompt and proceed automatically")
}

var rebuildCmd = &cobra.Command{
	Use:   "rebuild <profile>",
	Short: "Rebuild a VM while preserving session data",
	Long: `Rebuild the VM for the named profile by performing a backup, destroying the
existing VM, creating a fresh VM with the same configuration, and then
restoring the session data from the backup.

Steps:
  1. Backup session data from the running VM
  2. Destroy the VM
  3. Start a new VM with the configuration stored in config.yaml
  4. Restore session data from the backup created in step 1

After the rebuild completes you must re-authenticate inside the VM:
  cloister <profile>
  claude login`,
	Args: cobra.ExactArgs(1),
	RunE: runRebuild,
}

// runRebuild is the handler for the rebuild subcommand.
func runRebuild(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Load the profile configuration before proceeding so that we can detect
	// missing profiles early and surface a clear error message.
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	p, exists := cfg.Profiles[name]
	if !exists {
		return fmt.Errorf("profile %q not found", name)
	}

	if !rbf.yes {
		// Prompt for confirmation before taking any destructive action so the
		// user has a chance to abort if the command was run by mistake.
		cmd.Println("This will: backup → destroy → re-provision the VM.")
		cmd.Printf("All existing VM state will be replaced. Continue? [Y/n]: ")

		reader := bufio.NewReader(os.Stdin)
		answer, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading confirmation: %w", err)
		}
		answer = strings.TrimSpace(answer)
		if answer != "" && !strings.EqualFold(answer, "y") {
			cmd.Println("Rebuild cancelled.")
			return nil
		}
	}

	// Step 1: Back up session data while the VM is still running.
	cmd.Printf("Step 1/4: Backing up session data for %q...\n", name)
	backupPath, err := backup.Backup(name)
	if err != nil {
		return fmt.Errorf("backup before rebuild: %w", err)
	}
	cmd.Printf("  Backup saved: %s\n", backupPath)

	// Step 2: Destroy the existing VM. The --force flag allows deletion of a
	// running VM without requiring a prior stop.
	cmd.Printf("Step 2/4: Destroying VM for %q...\n", name)
	if err := vm.Delete(name, false); err != nil {
		return fmt.Errorf("deleting VM: %w", err)
	}

	// Step 3: Start a fresh VM using the profile's stored configuration. Full
	// provisioning (installing stacks, dotfiles, etc.) is wired in Task 16;
	// for now only the VM itself is created with the correct resource parameters.
	cmd.Printf("Step 3/4: Creating new VM for %q...\n", name)

	// Apply default resource values for any fields left at their zero value so
	// that the Colima call receives valid arguments.
	cpus := p.CPU
	if cpus == 0 {
		cpus = config.DefaultCPU
	}
	memGB := p.Memory
	if memGB == 0 {
		memGB = config.DefaultMemory
	}
	diskGB := p.Disk
	if diskGB == 0 {
		diskGB = config.DefaultDisk
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	mounts := vm.BuildMounts(home)

	if err := vm.Start(name, cpus, memGB, diskGB, mounts, false); err != nil {
		return fmt.Errorf("starting new VM: %w", err)
	}

	// Step 4: Restore session data from the backup captured in step 1.
	cmd.Printf("Step 4/4: Restoring session data for %q...\n", name)
	if err := backup.Restore(name, backupPath); err != nil {
		return fmt.Errorf("restoring backup: %w", err)
	}

	cmd.Printf("\nRebuild complete for profile %q.\n", name)
	cmd.Println("Run 'claude login' inside the VM to re-authenticate.")
	return nil
}
