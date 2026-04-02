package cmd

import (
	"fmt"
	"os"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

// snapshotForce controls whether the VM is stopped without a confirmation
// prompt when it is running at snapshot time.
var snapshotForce bool

var snapshotCmd = &cobra.Command{
	Use:   "snapshot <profile>",
	Short: "Create a restorable snapshot of a Lume VM",
	Long: `Save the current state of a Lume VM as a named snapshot. The VM is stopped
during the snapshot and restarted afterward. Use 'cloister reset' to restore.`,
	Args: cobra.ExactArgs(1),
	RunE: runSnapshot,
}

func init() {
	snapshotCmd.Flags().BoolVar(&snapshotForce, "force", false, "Stop the VM without prompting before taking the snapshot")
	rootCmd.AddCommand(snapshotCmd)
}

// runSnapshot stops the named profile's VM, captures a user snapshot via the
// GoldenImageManager, restarts the VM, and persists the snapshot metadata to
// the profile's state file.
func runSnapshot(cmd *cobra.Command, args []string) error {
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

	// Snapshots are only supported by backends that implement GoldenImageManager
	// (currently only the Lume backend). Colima profiles do not expose a clone-based
	// snapshot mechanism and should use 'cloister backup' instead.
	gim, ok := backend.(vm.GoldenImageManager)
	if !ok {
		return fmt.Errorf("profile %q does not support snapshots (only Lume profiles do)", name)
	}

	// Determine whether the VM is currently running so we know whether to stop
	// it before snapshotting and restart it afterward.
	wasRunning := backend.IsRunning(name)
	if wasRunning {
		if !snapshotForce && !promptYesNo(fmt.Sprintf("Profile %q is running. Stop it to take a snapshot? [Y/n]: ", name)) {
			fmt.Println("Snapshot cancelled.")
			return nil
		}
		fmt.Printf("Stopping %q for snapshot...\n", name)
		if err := backend.Stop(name, false); err != nil {
			return fmt.Errorf("stopping VM before snapshot: %w", err)
		}
	}

	// Capture the user snapshot. The snapshot is stored as a sibling Lume VM
	// named <vmName>-user, enabling fast clone-based resets via 'cloister reset'.
	fmt.Printf("Creating snapshot for %q...\n", name)
	if err := gim.Snapshot(name, "user"); err != nil {
		return fmt.Errorf("creating snapshot: %w", err)
	}

	// Restart the VM when it was running before the snapshot so the operator
	// experiences no net downtime beyond the snapshot window itself.
	if wasRunning {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}
		workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
		if err != nil {
			return fmt.Errorf("resolving workspace directory: %w", err)
		}
		mounts := vm.BuildMounts(home, workspaceDir, p.Stacks, p.MountPolicy, p.Headless)

		fmt.Printf("Restarting %q...\n", name)
		p.ApplyDefaults()
		if err := backend.Start(name, p.CPU, p.Memory, p.Disk, mounts, false); err != nil {
			return fmt.Errorf("restarting VM after snapshot: %w", err)
		}
	}

	// Persist the snapshot name and creation timestamp to the profile's state
	// file so that 'cloister status' and 'cloister reset' can reference it
	// without querying the Lume backend directly.
	state, err := vm.LoadProfileState(name)
	if err != nil {
		return fmt.Errorf("loading profile state: %w", err)
	}
	state.Snapshots.User = "user"
	state.Snapshots.UserCreated = vm.NowISO()
	if err := vm.SaveProfileState(name, state); err != nil {
		// Non-fatal: the snapshot itself succeeded; warn rather than aborting.
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: saving state after snapshot: %v\n", err)
	}

	fmt.Printf("Snapshot saved for %q. Restore with: cloister reset %s\n", name, name)
	return nil
}
