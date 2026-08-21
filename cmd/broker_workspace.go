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
	"cloister.io/internal/workspace"
)

var newWorkspaceBroker = func() (broker.SyncBroker, error) {
	return broker.NewMutagen()
}

func workspaceProvider(p *config.Profile) vm.WorkspaceProvider {
	if p != nil && p.Workspace.Mode == config.WorkspaceModeBroker {
		return vm.BrokerWorkspace
	}
	if p != nil && p.Workspace.Mode == config.WorkspaceModeWorkspace {
		return vm.WorkspaceBroker
	}
	return vm.VirtiofsWorkspace
}

func brokerLifecycle(backend vm.Backend, profile string, p *config.Profile) (*lifecycle.Coordinator, []broker.SessionSpec, error) {
	coordinator := lifecycle.NewCoordinator(backend)
	if !workspaceProvider(p).IsBroker() {
		return coordinator, nil, nil
	}
	specs, err := brokerSessionSpecs(backend, profile, p)
	if err != nil {
		return nil, nil, err
	}
	syncBroker, err := newWorkspaceBroker()
	if err != nil {
		return nil, nil, err
	}
	coordinator.Broker = syncBroker
	return coordinator, specs, nil
}

func brokerSessionSpec(backend vm.Backend, profile string, p *config.Profile) (*broker.SessionSpec, error) {
	specs, err := brokerSessionSpecs(backend, profile, p)
	if err != nil {
		return nil, err
	}
	if len(specs) != 1 {
		return nil, fmt.Errorf("profile %q has %d workspace projects; a single project was required", profile, len(specs))
	}
	return &specs[0], nil
}

func brokerSessionSpecs(backend vm.Backend, profile string, p *config.Profile) ([]broker.SessionSpec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}
	if workspaceProvider(p) == vm.WorkspaceBroker {
		return workspace.Discover(profile, p.StartDir, home, p.Workspace, backend.SSHConfig(profile))
	}
	root, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return nil, fmt.Errorf("resolving broker project root: %w", err)
	}
	spec, err := broker.BuildSessionSpec(profile, root, backend.SSHConfig(profile), p.Workspace.Ignore)
	if err != nil {
		return nil, err
	}
	return []broker.SessionSpec{spec}, nil
}

func ensureBrokerWorkspace(backend vm.Backend, profile string, p *config.Profile) error {
	coordinator, specs, err := brokerLifecycle(backend, profile, p)
	if err != nil || len(specs) == 0 {
		return err
	}
	return coordinator.ActivateBrokers(context.Background(), specs)
}

func quiesceBrokerWorkspace(backend vm.Backend, profile string, p *config.Profile, terminate bool) error {
	coordinator, specs, err := brokerLifecycle(backend, profile, p)
	if err != nil || len(specs) == 0 {
		return err
	}
	return coordinator.QuiesceBrokers(context.Background(), specs, terminate)
}

func stopVM(backend vm.Backend, profile string, p *config.Profile, terminate, verbose bool) error {
	coordinator, specs, err := brokerLifecycle(backend, profile, p)
	if err != nil {
		return err
	}
	return coordinator.StopBrokers(context.Background(), profile, specs, terminate, verbose)
}

func warnBrokerGitOnce(profile string, p *config.Profile) error {
	if !workspaceProvider(p).IsBroker() {
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
	fmt.Fprintln(os.Stderr, "The host .git directory is never copied into the VM. Guest git and gh commands are proxied to the host while a Cloister session is active.")
	return os.WriteFile(path, []byte("shown\n"), 0o600)
}
