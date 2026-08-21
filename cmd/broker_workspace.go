// Proprietary and confidential. All rights reserved.

package cmd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/lifecycle"
	"cloister.io/internal/vm"
)

var newWorkspaceBroker = func() (broker.SyncBroker, error) {
	return broker.NewMutagen()
}

func workspaceProvider(p *config.Profile) vm.WorkspaceProvider {
	if p != nil && p.Workspace.Mode == config.WorkspaceModeBroker {
		return vm.BrokerWorkspace
	}
	return vm.VirtiofsWorkspace
}

func brokerLifecycle(backend vm.Backend, profile string, p *config.Profile) (*lifecycle.Coordinator, *broker.SessionSpec, error) {
	coordinator := lifecycle.NewCoordinator(backend)
	if workspaceProvider(p) != vm.BrokerWorkspace {
		return coordinator, nil, nil
	}
	spec, err := brokerSessionSpec(backend, profile, p)
	if err != nil {
		return nil, nil, err
	}
	syncBroker, err := newWorkspaceBroker()
	if err != nil {
		return nil, nil, err
	}
	coordinator.Broker = syncBroker
	return coordinator, spec, nil
}

func brokerSessionSpec(backend vm.Backend, profile string, p *config.Profile) (*broker.SessionSpec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}
	root, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return nil, fmt.Errorf("resolving broker project root: %w", err)
	}
	spec, err := broker.BuildSessionSpec(profile, root, backend.SSHConfig(profile), p.Workspace.Ignore)
	if err != nil {
		return nil, err
	}
	return &spec, nil
}

func ensureBrokerWorkspace(backend vm.Backend, profile string, p *config.Profile) error {
	coordinator, spec, err := brokerLifecycle(backend, profile, p)
	if err != nil || spec == nil {
		return err
	}
	return coordinator.ActivateBroker(context.Background(), spec)
}

func quiesceBrokerWorkspace(backend vm.Backend, profile string, p *config.Profile, terminate bool) error {
	coordinator, spec, err := brokerLifecycle(backend, profile, p)
	if err != nil || spec == nil {
		return err
	}
	return coordinator.QuiesceBroker(context.Background(), spec, terminate)
}

func stopVM(backend vm.Backend, profile string, p *config.Profile, terminate, verbose bool) error {
	coordinator, spec, err := brokerLifecycle(backend, profile, p)
	if err != nil {
		return err
	}
	return coordinator.Stop(context.Background(), profile, spec, terminate, verbose)
}

func warnBrokerGitOnce(profile string, p *config.Profile) error {
	if workspaceProvider(p) != vm.BrokerWorkspace {
		return nil
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return err
	}
	warningDir := filepath.Join(dir, "state", "warnings")
	if err := os.MkdirAll(warningDir, 0o700); err != nil {
		return err
	}
	profileID := sha256.Sum256([]byte(profile))
	path := filepath.Join(warningDir, fmt.Sprintf("broker-git-%x", profileID[:8]))
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(os.Stderr, "Warning: workspace broker mode provides a synchronized copy, not local-filesystem equivalence.")
	fmt.Fprintln(os.Stderr, "The host .git directory is never copied into the VM, so in-guest git commands are unavailable. Run version-control operations on the host.")
	return os.WriteFile(path, []byte("shown\n"), 0o600)
}
