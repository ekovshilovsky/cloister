package cmd

import (
	"fmt"
	"os"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/lifecycle"
	"cloister.io/internal/vm"
	"cloister.io/internal/workspace"
)

// startVM starts the VM for a profile, wrapping backend.Start with detection and
// recovery for the stale disk-lock failure that follows an unclean shutdown
// (e.g. a host crash). All cloister start paths route through this helper so the
// behavior is consistent everywhere.
//
// On a start failure it asks the backend (if it supports it) to diagnose a stale
// lock. When one is found:
//   - Interactive terminal: explain the cause, prompt for confirmation, clear
//     the lock, and retry the start once.
//   - Non-interactive (agent/CI): explain the cause and point the user at
//     `cloister cleanup`, then return the original error without killing
//     anything.
//
// Any failure that is not a stale lock is returned unchanged.
func startVM(backend vm.Backend, profile string, p *config.Profile, extraSupplemental []vm.Mount, verbose bool) error {
	return startVMWithProvider(backend, profile, p, extraSupplemental, workspaceProvider(p), verbose)
}

func startVMWithProvider(backend vm.Backend, profile string, p *config.Profile, extraSupplemental []vm.Mount, provider vm.WorkspaceProvider, verbose bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return fmt.Errorf("resolving workspace directory: %w", err)
	}

	resolved := *p
	resolved.ApplyDefaults()
	if provider == vm.WorkspaceBroker && resolved.Workspace.Root != "" {
		workspaceDir, err = config.ResolveWorkspaceDir(resolved.Workspace.Root, home)
		if err != nil {
			return fmt.Errorf("resolving workspace routing root: %w", err)
		}
	}
	supplemental := vm.BuildSupplementalMounts(home, resolved.Stacks, resolved.MountPolicy, resolved.Headless)
	supplemental = append(supplemental, extraSupplemental...)

	coordinator := lifecycle.NewCoordinator(backend)
	var brokerSpecs []broker.SessionSpec
	if provider.IsBroker() {
		syncBroker, err := newWorkspaceBroker()
		if err != nil {
			return err
		}
		if provider == vm.WorkspaceBroker {
			brokerSpecs, err = workspace.Discover(profile, resolved.StartDir, home, resolved.Workspace, backend.SSHConfig(profile))
		} else {
			var spec broker.SessionSpec
			spec, err = broker.BuildSessionSpec(profile, workspaceDir, backend.SSHConfig(profile), resolved.Workspace.Ignore)
			brokerSpecs = []broker.SessionSpec{spec}
		}
		if err != nil {
			return err
		}
		coordinator.Broker = syncBroker
		if resolved.Agent != nil {
			if err := warnBrokerGitOnce(profile, &resolved); err != nil {
				return fmt.Errorf("recording workspace broker warning: %w", err)
			}
		}
	}
	coordinator.Recover = func(recoverer vm.StaleLockRecoverer, profile string, diag *vm.StaleLockDiagnosis) error {
		fmt.Fprintln(os.Stderr)
		fmt.Fprint(os.Stderr, diag.Summary)
		if !isInteractive() {
			fmt.Fprintln(os.Stderr, "Run 'cloister cleanup' to clear the stale lock, then retry.")
			return lifecycle.ErrRecoveryDeclined
		}
		if !promptYesNo("Recover now and retry the start? [Y/n] ") {
			fmt.Fprintln(os.Stderr, "Run 'cloister cleanup' to clear the stale lock, then retry.")
			return lifecycle.ErrRecoveryDeclined
		}
		cleared, err := recoverer.ClearStaleLock(profile)
		if err != nil {
			return fmt.Errorf("stale-lock recovery failed: %w", err)
		}
		fmt.Printf("Cleared stale lock (terminated %d orphaned process(es)). Retrying start...\n", cleared)
		return nil
	}

	return coordinator.Start(lifecycle.StartRequest{
		Profile:            profile,
		CPUs:               resolved.CPU,
		MemoryGB:           resolved.Memory,
		DiskGB:             resolved.Disk,
		RootDiskGB:         resolved.RootDisk,
		MountInotify:       resolved.MountInotify,
		SupplementalMounts: supplemental,
		WorkspaceDir:       workspaceDir,
		WorkspaceProvider:  provider,
		BrokerSpecs:        brokerSpecs,
		Verbose:            verbose,
	})
}
