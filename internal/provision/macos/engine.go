package macos

import (
	"fmt"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

type Engine struct{}

func (e *Engine) Run(profile string, p *config.Profile, backend vm.Backend) error {
	for _, step := range ProvisioningSteps() {
		fmt.Printf("  %s...\n", step.Name)
		if _, err := backend.SSHCommand(profile, step.Install); err != nil {
			return fmt.Errorf("%s: %w", step.Name, err)
		}
	}

	if p.Agent != nil && p.Agent.Type == "openclaw" {
		ds := DaemonStep()
		fmt.Printf("  %s...\n", ds.Name)
		if _, err := backend.SSHCommand(profile, ds.Install); err != nil {
			return fmt.Errorf("%s: %w", ds.Name, err)
		}
	}

	return nil
}

func (e *Engine) DeployConfig(profile string, p *config.Profile, backend vm.Backend) error {
	return nil
}
