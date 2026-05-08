package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
	vmcolima "github.com/ekovshilovsky/cloister/internal/vm/colima"
	"github.com/spf13/cobra"
)

// resizeFlags holds flag state for the resize subcommand.
type resizeFlags struct {
	yes bool
}

var rzf resizeFlags

func init() {
	rootCmd.AddCommand(resizeCmd)
	resizeCmd.Flags().BoolVarP(&rzf.yes, "yes", "y", false, "Skip the confirmation prompt and proceed automatically")
}

var resizeCmd = &cobra.Command{
	Use:   "resize <profile>",
	Short: "Grow a profile's root disk to match its configured size",
	Long: `Resize the root disk of an existing Colima VM to match the profile's
configured root_disk value. This is required when a VM was created before
cloister exposed the root_disk field — Colima's built-in default is 20 GiB,
which is too small for stack-heavy profiles — or after manually increasing
root_disk in config.yaml.

The operation is non-destructive:
  - the VM is stopped
  - the raw disk image is preserved as disk.bak via APFS reflink (instant,
    zero additional space; restored on failure)
  - the image is sparse-extended to the target size
  - colima.yaml and lima.yaml are updated to declare the new size
  - the VM is restarted; Lima's first-boot logic grows vda1 and resize2fs
    automatically — no in-VM intervention is required

Resizes only grow the root disk. Shrinking is refused.`,
	Args: cobra.ExactArgs(1),
	RunE: runResize,
}

func runResize(cmd *cobra.Command, args []string) error {
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
	p.ApplyDefaults()

	if p.Backend == "lume" {
		return fmt.Errorf("resize not supported for Lume profiles; use 'cloister rebuild %s' instead", name)
	}

	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	actual, err := vmcolima.RootDiskGB(name)
	if err != nil {
		return fmt.Errorf("reading current root disk size: %w", err)
	}
	if actual == 0 {
		return fmt.Errorf("rootDisk not recorded in colima.yaml for profile %q (was the VM ever started?)", name)
	}

	target := p.RootDisk
	if actual >= target {
		cmd.Printf("No drift: profile %q root disk is already %d GiB (config wants %d GiB).\n", name, actual, target)
		return nil
	}

	cmd.Printf("Profile %q root disk drift detected:\n", name)
	cmd.Printf("  current: %d GiB\n", actual)
	cmd.Printf("  target : %d GiB\n", target)
	cmd.Println()
	cmd.Println("Resize will: stop the VM, extend the raw disk image, update both colima.yaml")
	cmd.Println("and lima.yaml, and restart the VM. The original image is preserved as disk.bak")
	cmd.Println("via APFS clone until the resize completes successfully.")
	cmd.Println()

	if !rzf.yes {
		cmd.Print("Proceed? [Y/n]: ")
		reader := bufio.NewReader(os.Stdin)
		ans, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading confirmation: %w", err)
		}
		ans = strings.TrimSpace(strings.ToLower(ans))
		if ans != "" && ans != "y" {
			cmd.Println("Resize cancelled.")
			return nil
		}
	}

	wasRunning := backend.IsRunning(name)
	if wasRunning {
		cmd.Printf("Stopping %q...\n", name)
		if err := backend.Stop(name, false); err != nil {
			return fmt.Errorf("stopping VM: %w", err)
		}
	}

	cmd.Printf("Extending root disk: %d → %d GiB...\n", actual, target)
	if err := vmcolima.ResizeRootDiskFile(name, target); err != nil {
		return fmt.Errorf("resizing root disk: %w", err)
	}

	// A boot is required so Lima's first-boot logic grows vda1 and runs
	// resize2fs over the extended image. The boot is unconditional even
	// when the VM was originally stopped, because the partition table and
	// filesystem must be brought up to the new size before the resize is
	// considered complete; pre-resize stopped state is restored below.
	if wasRunning {
		cmd.Printf("Restarting %q...\n", name)
	} else {
		cmd.Printf("Booting %q to apply partition/filesystem grow...\n", name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return fmt.Errorf("invalid workspace directory: %w", err)
	}
	mounts := vm.BuildMounts(home, workspaceDir, p.Stacks, p.MountPolicy, p.Headless)

	if err := backend.Start(name, p.CPU, p.Memory, p.Disk, p.RootDisk, p.MountInotify, mounts, false); err != nil {
		return fmt.Errorf("starting VM after resize: %w (disk.bak preserved for rollback)", err)
	}

	// Restore pre-resize lifecycle state so a profile that was stopped
	// before the resize remains stopped afterward. By the time Start
	// returns, Lima's boot-time grow has already completed.
	if !wasRunning {
		cmd.Printf("Stopping %q (partition grow complete; restoring pre-resize stopped state)...\n", name)
		if err := backend.Stop(name, false); err != nil {
			return fmt.Errorf("stopping VM after partition grow: %w", err)
		}
	}

	if err := vmcolima.CleanupResizeBackup(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove disk.bak: %v\n", err)
	}

	finalState := "stopped"
	if wasRunning {
		finalState = "running"
	}
	cmd.Printf("\nResize complete. Profile %q is %s (matches pre-resize state).\n", name, finalState)
	cmd.Printf("Verify size with: cloister exec %s -- df -h /\n", name)
	return nil
}
