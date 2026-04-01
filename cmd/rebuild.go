package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/backup"
	"github.com/ekovshilovsky/cloister/internal/config"
	macosprov "github.com/ekovshilovsky/cloister/internal/provision/macos"
	"github.com/ekovshilovsky/cloister/internal/provision"
	"github.com/ekovshilovsky/cloister/internal/vm"
	vmlume "github.com/ekovshilovsky/cloister/internal/vm/lume"
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
restoring the session data from the backup.`,
	Args: cobra.ExactArgs(1),
	RunE: runRebuild,
}

// runRebuild is the handler for the rebuild subcommand.
func runRebuild(cmd *cobra.Command, args []string) error {
	name := args[0]

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

	// Resolve the backend for this profile so that VM operations and backup
	// streaming use the correct hypervisor implementation.
	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	// Step 1: Back up session data while the VM is still running.
	var backupPath string
	if backend.IsRunning(name) {
		cmd.Printf("Step 1/4: Backing up session data for %q...\n", name)
		bp, err := backup.Backup(name, backend)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: backup failed: %v\n", err)
			fmt.Fprintf(os.Stderr, "  Continuing rebuild without backup.\n")
		} else {
			backupPath = bp
			cmd.Printf("  Backup saved: %s\n", backupPath)
		}
	} else if backend.Exists(name) {
		cmd.Printf("Step 1/4: VM not running, skipping backup.\n")
	} else {
		cmd.Printf("Step 1/4: No existing VM, skipping backup.\n")
	}

	if backend.Exists(name) {
		cmd.Printf("Step 2/4: Destroying VM for %q...\n", name)
		if err := backend.Delete(name, false); err != nil {
			return fmt.Errorf("deleting VM: %w", err)
		}
	} else {
		cmd.Printf("Step 2/4: No VM to destroy, proceeding.\n")
	}

	cmd.Printf("Step 3/4: Creating and provisioning new VM for %q...\n", name)

	if p.Backend == "lume" {
		if err := rebuildLumeProfile(name, p, backend); err != nil {
			return fmt.Errorf("rebuilding Lume profile: %w", err)
		}
	} else {
		if err := rebuildColimaProfile(name, p, backend); err != nil {
			return fmt.Errorf("rebuilding Colima profile: %w", err)
		}
	}

	if backupPath != "" {
		cmd.Printf("Step 4/4: Restoring session data for %q...\n", name)
		if err := backup.Restore(name, backupPath, backend); err != nil {
			return fmt.Errorf("restoring backup: %w", err)
		}
	} else {
		cmd.Printf("Step 4/4: No backup to restore.\n")
	}

	cmd.Printf("\nRebuild complete for profile %q.\n", name)
	return nil
}

func rebuildLumeProfile(name string, p *config.Profile, backend vm.Backend) error {
	gim, ok := backend.(vm.GoldenImageManager)
	if !ok {
		return fmt.Errorf("backend does not support golden image management")
	}

	if !gim.BaseExists() {
		if err := vmlume.CheckHostCompatibility(); err != nil {
			return err
		}
		fmt.Println("  Base image missing — creating (15-20 minutes)...")
		if err := gim.CreateBase(true, ""); err != nil {
			return fmt.Errorf("creating base image: %w", err)
		}
	}

	vmName := backend.VMName(name)
	fmt.Println("  Cloning base image...")
	if err := gim.Clone(vmlume.BaseImageName, vmName); err != nil {
		return fmt.Errorf("cloning base image: %w", err)
	}

	p.ApplyDefaults()
	enforceMacOSMinimums(p)

	home, _ := os.UserHomeDir()
	workspaceDir, _ := config.ResolveWorkspaceDir(p.StartDir, home)
	mounts := vm.BuildMounts(home, vm.VMHome(name), workspaceDir, p.Stacks, p.MountPolicy, p.Headless)

	fmt.Println("  Starting VM...")
	if err := backend.Start(name, p.CPU, p.Memory, p.Disk, mounts, false); err != nil {
		return fmt.Errorf("starting VM: %w", err)
	}

	if err := waitForLumeReady(vmName, 120); err != nil {
		return fmt.Errorf("VM did not become ready: %w", err)
	}

	fmt.Println("  Deploying SSH key...")
	_, pubKey, err := vmlume.GenerateKey(name)
	if err != nil {
		return fmt.Errorf("generating SSH key: %w", err)
	}
	if err := vmlume.DeployKey(vmName, pubKey); err != nil {
		return fmt.Errorf("deploying SSH key: %w", err)
	}

	if _, err := backend.SSHCommand(name, "echo ok"); err != nil {
		return fmt.Errorf("SSH key verification failed: %w", err)
	}

	lumeBackend, _ := backend.(*vmlume.Backend)
	fmt.Println("  Configuring hostname...")
	if err := vmlume.SetHostname(name, lumeBackend); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: hostname setup failed: %v\n", err)
	}

	fmt.Println("  Provisioning...")
	macosEngine := &macosprov.Engine{}
	if err := macosEngine.Run(name, p, backend); err != nil {
		return fmt.Errorf("provisioning: %w", err)
	}

	fmt.Println("  Creating factory snapshot...")
	if err := backend.Stop(name, false); err != nil {
		return fmt.Errorf("stopping VM for snapshot: %w", err)
	}
	if err := gim.Snapshot(name, "factory"); err != nil {
		return fmt.Errorf("creating factory snapshot: %w", err)
	}

	if err := backend.Start(name, p.CPU, p.Memory, p.Disk, mounts, false); err != nil {
		return fmt.Errorf("restarting VM: %w", err)
	}

	state := &vm.ProfileState{
		Backend: "lume",
		VM:      vm.VMState{Hostname: vmlume.Hostname(name)},
		Snapshots: vm.SnapshotState{
			Factory:        fmt.Sprintf("%s-factory", vmName),
			FactoryCreated: vm.NowISO(),
		},
		BaseImage: vm.BaseImageState{
			Name:    vmlume.BaseImageName,
			Created: vm.NowISO(),
		},
	}
	if natNet, ok := backend.(vm.NATNetworker); ok {
		if ip, err := natNet.VMIP(name); err == nil {
			state.VM.IP = ip
		}
	}
	_ = vm.SaveProfileState(name, state)

	return nil
}

func rebuildColimaProfile(name string, p *config.Profile, backend vm.Backend) error {
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
	workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return fmt.Errorf("invalid workspace directory: %w", err)
	}
	mounts := vm.BuildMounts(home, vm.VMHome(name), workspaceDir, p.Stacks, p.MountPolicy, p.Headless)

	if err := backend.Start(name, cpus, memGB, diskGB, mounts, false); err != nil {
		return fmt.Errorf("starting VM: %w", err)
	}

	return provision.Run(name, p)
}
