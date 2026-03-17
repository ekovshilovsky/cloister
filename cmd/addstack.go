package cmd

import (
	"fmt"
	"os"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/profile"
	"github.com/ekovshilovsky/cloister/internal/provision"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(addStackCmd)
}

var addStackCmd = &cobra.Command{
	Use:   "add-stack <profile> <stack>",
	Short: "Add a provisioning stack to an existing profile",
	Args:  cobra.ExactArgs(2),
	RunE:  runAddStack,
}

// runAddStack installs a named toolchain stack into the VM for an existing
// profile, updating the persisted configuration so the stack is retained
// across rebuilds.
func runAddStack(cmd *cobra.Command, args []string) error {
	profileName := args[0]
	stackName := args[1]

	// Validate the requested stack name against the set of supported stacks
	// before performing any I/O or VM operations.
	if err := profile.ValidateStacks([]string{stackName}); err != nil {
		return err
	}

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

	// Prevent duplicate stack entries to avoid running the provisioning script
	// more than once for the same stack.
	for _, s := range p.Stacks {
		if s == stackName {
			return fmt.Errorf("stack %q is already installed in %q", stackName, profileName)
		}
	}

	// Ensure the VM is running before attempting to execute the provisioning
	// script over SSH. Start it with the profile's stored resource parameters
	// when it is currently stopped.
	if !vm.IsRunning(profileName) {
		fmt.Printf("Starting %q...\n", profileName)
		home, _ := os.UserHomeDir()
		mounts := vm.BuildMounts(home)
		p.ApplyDefaults()
		if err := vm.Start(profileName, p.CPU, p.Memory, p.Disk, mounts, false); err != nil {
			return fmt.Errorf("failed to start: %w", err)
		}
	}

	// Read the embedded stack provisioning script and execute it inside the VM
	// via a non-interactive SSH session.
	fmt.Printf("Installing %q stack in %q...\n", stackName, profileName)
	scriptPath := fmt.Sprintf("scripts/stack-%s.sh", stackName)
	scriptData, err := provision.Scripts.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("reading stack script: %w", err)
	}
	if _, err := vm.SSHCommand(profileName, string(scriptData)); err != nil {
		return fmt.Errorf("stack installation failed: %w", err)
	}

	// Append the stack to the profile's Stacks list and persist the updated
	// configuration so subsequent commands (e.g. rebuild) reproduce the same
	// set of stacks.
	p.Stacks = append(p.Stacks, stackName)
	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}

	fmt.Printf("✓ Stack %q added to %q\n", stackName, profileName)
	return nil
}
