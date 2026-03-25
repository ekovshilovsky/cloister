package cmd

import (
	"fmt"
	"os"

	"github.com/ekovshilovsky/cloister/internal/agent"
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

// resetFactory selects the factory (post-provisioning) snapshot as the restore
// target instead of the most recent user snapshot.
var resetFactory bool

var resetCmd = &cobra.Command{
	Use:   "reset <profile>",
	Short: "Reset a Lume VM to a snapshot",
	Long: `Destroy the current VM and restore from a snapshot. Defaults to the most
recent user snapshot. Use --factory to reset to the post-provisioning state.`,
	Args: cobra.ExactArgs(1),
	RunE: runReset,
}

func init() {
	resetCmd.Flags().BoolVar(&resetFactory, "factory", false, "Reset to factory snapshot instead of user snapshot")
	rootCmd.AddCommand(resetCmd)
}

// runReset reverts a Lume VM to a prior snapshot state. When --factory is set
// the post-provisioning clone is used; otherwise the most recent user
// checkpoint is the restore target. Any active SSH tunnels are torn down before
// the VM is destroyed so that stale forwarding processes do not outlive the
// instance they were connected to.
func runReset(cmd *cobra.Command, args []string) error {
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

	// Resets are only supported by backends that implement GoldenImageManager
	// (currently only the Lume backend). Colima profiles do not expose a
	// clone-based snapshot mechanism and should use 'cloister rebuild' instead.
	gim, ok := backend.(vm.GoldenImageManager)
	if !ok {
		return fmt.Errorf("profile %q does not support reset (only Lume profiles do)", name)
	}

	// Stop the VM and close all active SSH port-forward tunnels before
	// destroying the instance, preventing stale forwarding processes from
	// holding open connections to a VM that no longer exists.
	if backend.IsRunning(name) {
		fmt.Printf("Stopping %q...\n", name)
		if err := backend.Stop(name, false); err != nil {
			return fmt.Errorf("stopping VM before reset: %w", err)
		}
	}
	agent.CloseAllForwards(name)

	// Load the persisted state to check whether a user snapshot already exists.
	// When none does and --factory was not requested, capture a safety snapshot
	// before destroying the current VM so the user can always recover their work.
	state, err := vm.LoadProfileState(name)
	if err != nil {
		return fmt.Errorf("loading profile state: %w", err)
	}

	if !resetFactory && state.Snapshots.User == "" {
		fmt.Println("No user snapshot exists. Creating one before reset...")
		if err := gim.Snapshot(name, "user"); err != nil {
			return fmt.Errorf("creating safety snapshot before reset: %w", err)
		}
		state.Snapshots.User = "user"
		state.Snapshots.UserCreated = vm.NowISO()
		if err := vm.SaveProfileState(name, state); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: saving state after safety snapshot: %v\n", err)
		}
	}

	// Destroy the current VM and restore from the selected snapshot. The
	// GoldenImageManager.Reset implementation verifies that the target snapshot
	// exists before deleting the current instance, preserving safety.
	snapshotLabel := "user"
	if resetFactory {
		snapshotLabel = "factory"
	}
	fmt.Printf("Resetting %q to %s snapshot...\n", name, snapshotLabel)
	if err := gim.Reset(name, resetFactory); err != nil {
		return fmt.Errorf("resetting VM: %w", err)
	}

	// Rebuild the mount list using the current profile configuration and
	// restart the VM so it is immediately usable after the reset completes.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return fmt.Errorf("resolving workspace directory: %w", err)
	}
	mounts := vm.BuildMounts(home, workspaceDir, p.Stacks, p.MountPolicy, p.Headless)

	fmt.Printf("Starting %q...\n", name)
	p.ApplyDefaults()
	if err := backend.Start(name, p.CPU, p.Memory, p.Disk, mounts, false); err != nil {
		return fmt.Errorf("starting VM after reset: %w", err)
	}

	// Update the state file to reflect the post-reset configuration. The VM
	// hostname and backend remain unchanged; only the snapshot metadata needs
	// to reflect which restore point was applied.
	state.Backend = p.Backend
	if resetFactory {
		// A factory reset clears the user snapshot record because the VM state
		// is now earlier than any user checkpoint that was captured previously.
		state.Snapshots.User = ""
		state.Snapshots.UserCreated = ""
	}
	if err := vm.SaveProfileState(name, state); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: saving state after reset: %v\n", err)
	}

	fmt.Printf("Reset complete. VM restored from %s snapshot.\n", snapshotLabel)
	return nil
}
