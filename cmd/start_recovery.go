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
	return startVMAtPath(backend, profile, p, extraSupplemental, "", verbose)
}

func startVMAtPath(backend vm.Backend, profile string, p *config.Profile, extraSupplemental []vm.Mount, projectRoot string, verbose bool) error {
	return startVMWithWorkspace(backend, profile, p, extraSupplemental, workspaceProvider(p), projectRoot, verbose)
}

func startVMWithProvider(backend vm.Backend, profile string, p *config.Profile, extraSupplemental []vm.Mount, provider vm.WorkspaceProvider, verbose bool) error {
	return startVMWithWorkspace(backend, profile, p, extraSupplemental, provider, "", verbose)
}

func startVMWithWorkspace(backend vm.Backend, profile string, p *config.Profile, extraSupplemental []vm.Mount, provider vm.WorkspaceProvider, projectRoot string, verbose bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return fmt.Errorf("resolving workspace directory: %w", err)
	}
	if provider == vm.BrokerWorkspace && projectRoot != "" {
		workspaceDir = projectRoot
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
	var brokerSpec *broker.SessionSpec
	var brokerSpecs []broker.SessionSpec
	if provider.IsBroker() {
		syncBroker, err := newWorkspaceBroker()
		if err != nil {
			return err
		}
		if provider == vm.WorkspaceBroker {
			if projectRoot == "" {
				var discovered []broker.SessionSpec
				discovered, err = workspace.Discover(profile, resolved.StartDir, home, resolved.Workspace, backend.SSHConfig(profile))
				brokerSpecs = append(make([]broker.SessionSpec, 0, len(discovered)), discovered...)
			} else {
				var spec broker.SessionSpec
				spec, err = workspace.ProjectSession(profile, projectRoot, resolved.StartDir, home, resolved.Workspace, backend.SSHConfig(profile))
				brokerSpec = &spec
			}
		} else {
			var spec broker.SessionSpec
			spec, err = broker.BuildSessionSpec(profile, workspaceDir, backend.SSHConfig(profile), resolved.Workspace.Ignore)
			brokerSpec = &spec
		}
		if err != nil {
			return err
		}
		coordinator.Broker = syncBroker
		// The start runs the metadata preflight for every project, so the
		// record has to exist before it does.
		defer attachPreflightLog(coordinator, profile)()
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
		BrokerSpec:         brokerSpec,
		BrokerSpecs:        brokerSpecs,
		Verbose:            verbose,
		AllowLowFDHeadroom: allowLowFDHeadroom(),
	})
}

// allowLowFDHeadroom lets an operator bypass the pre-start descriptor guard on a
// host they know is safe. It is an env escape hatch rather than a per-command
// flag so every start path honors it without duplicating flag plumbing.
func allowLowFDHeadroom() bool {
	switch os.Getenv("CLOISTER_ALLOW_LOW_FD_HEADROOM") {
	case "", "0", "false", "FALSE", "no":
		return false
	default:
		return true
	}
}
