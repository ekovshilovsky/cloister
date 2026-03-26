package macos

import (
	"fmt"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

type Engine struct{}

func (e *Engine) Run(profile string, p *config.Profile, backend vm.Backend) error {
	phases := []struct {
		name  string
		steps []Step
	}{
		{"Preflight", PreflightSteps()},
		{"Provisioning", ProvisioningSteps()},
		{"Hardening", HardeningSteps()},
	}

	for _, phase := range phases {
		for _, step := range phase.steps {
			fmt.Printf("  %s...\n", step.Name)
			if _, err := backend.SSHCommand(profile, step.Install); err != nil {
				return fmt.Errorf("%s: %w", step.Name, err)
			}
		}
	}

	if p.Agent != nil && p.Agent.Type == "openclaw" {
		for _, step := range []Step{DaemonStep(), NodeHostStep()} {
			fmt.Printf("  %s...\n", step.Name)
			if _, err := backend.SSHCommand(profile, step.Install); err != nil {
				return fmt.Errorf("%s: %w", step.Name, err)
			}
		}
	}

	return nil
}

func (e *Engine) DeployConfig(profile string, p *config.Profile, backend vm.Backend) error {
	return nil
}
