package cmd

import (
	"fmt"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/agent"
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/tunnel"
	vmcolima "github.com/ekovshilovsky/cloister/internal/vm/colima"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(stopCmd)
}

var stopCmd = &cobra.Command{
	Use:   "stop <profile|all>",
	Short: "Stop a running profile VM",
	Long: `Stop the environment for the named profile.

Pass "all" to stop every running profile VM in one operation. Stopping an
already-stopped VM is a no-op and does not return an error.`,
	Args: cobra.ExactArgs(1),
	RunE: runStop,
}

// runStop is the handler for the stop subcommand.
func runStop(cmd *cobra.Command, args []string) error {
	target := args[0]

	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if strings.EqualFold(target, "all") {
		return stopAll(cfg)
	}

	return stopOne(cfg, target)
}

// stopAll iterates every profile in the configuration and stops any that
// currently have a running VM. Errors from individual stop operations are
// collected and reported together so that one failure does not prevent the
// remaining profiles from being stopped.
func stopAll(cfg *config.Config) error {
	// Use colima backend directly to list colima VMs. In the future, both
	// backends would be queried when listing all managed VMs.
	backend := &vmcolima.Backend{}
	vmList, err := backend.List(false)
	if err != nil {
		return fmt.Errorf("querying VM state: %w", err)
	}

	runningByProfile := make(map[string]bool, len(vmList))
	for _, s := range vmList {
		pName := backend.ProfileFromVMName(s.Name)
		if pName != "" && strings.EqualFold(s.Status, "running") {
			runningByProfile[pName] = true
		}
	}

	var lastErr error
	for name, p := range cfg.Profiles {
		if !runningByProfile[name] {
			continue
		}

		profileBackend, err := resolveBackend(p.Backend)
		if err != nil {
			fmt.Printf("error resolving backend for %q: %v\n", name, err)
			lastErr = err
			continue
		}

		fmt.Printf("Stopping %q...\n", name)
		agent.CloseAllForwards(name)
		tunnel.StopAll(name)
		if err := profileBackend.Stop(name, false); err != nil {
			fmt.Printf("error stopping %q: %v\n", name, err)
			lastErr = err
			continue
		}
		fmt.Printf("Stopped %q\n", name)
	}

	return lastErr
}

// stopOne stops the VM for a single named profile. The operation is idempotent:
// if the VM is already stopped, the function returns without an error.
func stopOne(cfg *config.Config, name string) error {
	p, ok := cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}

	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	// Use IsRunning to determine whether the VM is active before attempting to
	// stop it, providing a clear no-op path for already-stopped profiles.
	if !backend.IsRunning(name) {
		fmt.Printf("Profile %q is not running\n", name)
		return nil
	}

	fmt.Printf("Stopping %q...\n", name)
	agent.CloseAllForwards(name)
	tunnel.StopAll(name)
	if err := backend.Stop(name, false); err != nil {
		return fmt.Errorf("stopping VM for profile %q: %w", name, err)
	}

	fmt.Printf("Stopped %q\n", name)
	return nil
}
