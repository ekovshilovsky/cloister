package macos

import (
	"fmt"
	"io"

	"cloister.io/internal/config"
	"cloister.io/internal/provision/report"
	"cloister.io/internal/vm"
)

type Engine struct {
	// Steps is where Run reports its progress. A nil Steps reports nothing and
	// discards the guest output, which is what this sequence did before it had
	// anywhere to put it.
	Steps report.Reporter
}

// steps is the progress destination, defaulting to no reporting at all.
func (e *Engine) steps() report.Reporter {
	if e == nil || e.Steps == nil {
		return report.Discarded{}
	}
	return e.Steps
}

func (e *Engine) Run(profile string, p *config.Profile, backend vm.Backend) error {
	steps := e.steps()

	installSteps := append(append(append([]Step(nil),
		PreflightSteps()...),
		ProvisioningSteps()...),
		HardeningSteps()...)

	if p.Agent != nil && p.Agent.Type == "openclaw" {
		installSteps = append(installSteps, DaemonStep(), OllamaProviderStep(), NodeHostStep())
	}

	for _, install := range installSteps {
		step := steps.Step(install.Name)
		// SSHCommand captures rather than streams, so the guest output has to be
		// handed on deliberately. Without this a failed macOS provision reports
		// the step that broke and nothing about why.
		out, err := backend.SSHCommand(profile, install.Install)
		_, _ = io.WriteString(step.Writer(), out)
		if err != nil {
			step.Fail()
			return fmt.Errorf("%s: %w", install.Name, err)
		}
		step.Done()
	}

	return nil
}

func (e *Engine) DeployConfig(profile string, p *config.Profile, backend vm.Backend) error {
	return nil
}
