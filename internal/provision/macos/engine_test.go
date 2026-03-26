package macos

import (
	"errors"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

func TestEngine_Run_CallSequence(t *testing.T) {
	t.Parallel()

	allSteps := append(PreflightSteps(), ProvisioningSteps()...)
	allSteps = append(allSteps, HardeningSteps()...)

	t.Run("non_agent_profile", func(t *testing.T) {
		t.Parallel()
		mock := &vm.MockBackend{}
		e := &Engine{}
		p := &config.Profile{}

		if err := e.Run("test-profile", p, mock); err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}

		if len(mock.SSHCommandCalls) != len(allSteps) {
			t.Fatalf("SSHCommand call count = %d, want %d", len(mock.SSHCommandCalls), len(allSteps))
		}

		for i, step := range allSteps {
			got := mock.SSHCommandCalls[i]
			if got.Profile != "test-profile" {
				t.Errorf("call[%d].Profile = %q, want %q", i, got.Profile, "test-profile")
			}
			if got.Command != step.Install {
				t.Errorf("call[%d].Command mismatch for %q", i, step.Name)
			}
		}
	})

	t.Run("openclaw_agent_profile", func(t *testing.T) {
		t.Parallel()
		mock := &vm.MockBackend{}
		e := &Engine{}
		p := &config.Profile{Agent: &config.AgentConfig{Type: "openclaw"}}

		if err := e.Run("openclaw-profile", p, mock); err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}

		wantCount := len(allSteps) + 1
		if len(mock.SSHCommandCalls) != wantCount {
			t.Fatalf("SSHCommand call count = %d, want %d", len(mock.SSHCommandCalls), wantCount)
		}

		last := mock.SSHCommandCalls[len(mock.SSHCommandCalls)-1]
		if last.Command != DaemonStep().Install {
			t.Errorf("final SSHCommand = %q, want daemon step", last.Command)
		}
	})
}

func TestEngine_Run_StopsOnError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("ssh failure")
	mock := &vm.MockBackend{SSHCommandErr: sentinel}
	e := &Engine{}
	p := &config.Profile{}

	err := e.Run("test-profile", p, mock)

	if err == nil {
		t.Fatal("Run should return an error when SSHCommand fails")
	}

	if !errors.Is(err, sentinel) {
		t.Errorf("Run error = %v; want it to wrap %v", err, sentinel)
	}

	if len(mock.SSHCommandCalls) != 1 {
		t.Errorf("SSHCommand call count = %d, want 1 (Run must stop at first error)", len(mock.SSHCommandCalls))
	}
}

func TestPhaseFunctions_ReturnSteps(t *testing.T) {
	t.Parallel()
	if len(PreflightSteps()) == 0 {
		t.Error("PreflightSteps() returned empty")
	}
	if len(ProvisioningSteps()) == 0 {
		t.Error("ProvisioningSteps() returned empty")
	}
	if len(HardeningSteps()) == 0 {
		t.Error("HardeningSteps() returned empty")
	}
}

func TestEngine_DeployConfig_IsNoOp(t *testing.T) {
	t.Parallel()

	mock := &vm.MockBackend{}
	e := &Engine{}
	p := &config.Profile{}

	if err := e.DeployConfig("test-profile", p, mock); err != nil {
		t.Fatalf("DeployConfig returned unexpected error: %v", err)
	}

	if len(mock.SSHCommandCalls) != 0 {
		t.Errorf("DeployConfig issued %d SSHCommand calls, want 0", len(mock.SSHCommandCalls))
	}
}

func TestEngine_Run_AllPhases(t *testing.T) {
	t.Parallel()
	mock := &vm.MockBackend{}
	e := &Engine{}
	p := &config.Profile{Agent: &config.AgentConfig{Type: "openclaw"}}

	if err := e.Run("test", p, mock); err != nil {
		t.Fatalf("Run: %v", err)
	}

	expectedCount := len(PreflightSteps()) + len(ProvisioningSteps()) + len(HardeningSteps()) + 1
	if len(mock.SSHCommandCalls) != expectedCount {
		t.Errorf("SSHCommand calls = %d, want %d", len(mock.SSHCommandCalls), expectedCount)
	}

	// Verify first call is preflight DNS
	first := mock.SSHCommandCalls[0]
	if first.Command != PreflightSteps()[0].Install {
		t.Errorf("first call should be preflight DNS, got %q", first.Command)
	}
}
