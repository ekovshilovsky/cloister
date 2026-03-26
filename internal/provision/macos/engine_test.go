package macos

import (
	"errors"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

func TestEngine_Run_CallSequence(t *testing.T) {
	t.Parallel()

	steps := ProvisioningSteps()

	t.Run("non_agent_profile", func(t *testing.T) {
		t.Parallel()

		mock := &vm.MockBackend{}
		e := &Engine{}
		p := &config.Profile{}

		if err := e.Run("test-profile", p, mock); err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}

		if len(mock.SSHCommandCalls) != len(steps) {
			t.Fatalf("SSHCommand call count = %d, want %d", len(mock.SSHCommandCalls), len(steps))
		}

		for i, step := range steps {
			got := mock.SSHCommandCalls[i]
			if got.Profile != "test-profile" {
				t.Errorf("call[%d].Profile = %q, want %q", i, got.Profile, "test-profile")
			}
			if got.Command != step.Install {
				t.Errorf("call[%d].Command = %q, want %q", i, got.Command, step.Install)
			}
		}
	})

	t.Run("openclaw_agent_profile", func(t *testing.T) {
		t.Parallel()

		mock := &vm.MockBackend{}
		e := &Engine{}
		p := &config.Profile{
			Agent: &config.AgentConfig{Type: "openclaw"},
		}

		if err := e.Run("openclaw-profile", p, mock); err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}

		wantCount := len(steps) + 1
		if len(mock.SSHCommandCalls) != wantCount {
			t.Fatalf("SSHCommand call count = %d, want %d", len(mock.SSHCommandCalls), wantCount)
		}

		last := mock.SSHCommandCalls[len(mock.SSHCommandCalls)-1]
		wantDaemon := DaemonStep().Install
		if last.Command != wantDaemon {
			t.Errorf("final SSHCommand = %q, want %q", last.Command, wantDaemon)
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
