package cmd

import (
	"fmt"
	"strings"

	"cloister.io/internal/agent"
	"cloister.io/internal/config"
	"cloister.io/internal/tunnel"
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
	var lastErr error
	for name, p := range cfg.Profiles {
		profileBackend, err := resolveBackend(p.Backend)
		if err != nil {
			fmt.Printf("error resolving backend for %q: %v\n", name, err)
			lastErr = err
			continue
		}

		if !profileBackend.IsRunning(name) {
			continue
		}

		fmt.Printf("Stopping %q...\n", name)
		if err := stopVM(profileBackend, name, p, false, false); err != nil {
			fmt.Printf("error stopping %q: %v\n", name, err)
			lastErr = err
			continue
		}
		agent.DropAllForwards(name)
		tunnel.StopAll(name)
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
	if err := stopVM(backend, name, p, false, false); err != nil {
		return fmt.Errorf("stopping VM for profile %q: %w", name, err)
	}
	agent.DropAllForwards(name)
	tunnel.StopAll(name)

	fmt.Printf("Stopped %q\n", name)
	return nil
}
