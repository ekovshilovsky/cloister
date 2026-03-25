package macos

import (
	"errors"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// expectedCommands enumerates the SSH commands that Run must issue in order
// for a standard (non-agent) macOS provisioning sequence. The daemon
// installation step is absent because it only runs for openclaw agent profiles.
var expectedCommands = []string{
	"xcode-select --install 2>/dev/null || true; until xcode-select -p &>/dev/null; do sleep 5; done",
	`NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`,
	`echo 'eval "$(/opt/homebrew/bin/brew shellenv)"' >> ~/.zprofile && eval "$(/opt/homebrew/bin/brew shellenv)"`,
	"/opt/homebrew/bin/brew install node",
	"npm install -g openclaw@latest",
}

// TestEngine_Run_CallSequence verifies that Run issues SSHCommand calls in the
// correct order with the exact command strings defined by the provisioning
// sequence. It also verifies that the daemon installation step is appended for
// profiles whose Agent.Type is "openclaw".
func TestEngine_Run_CallSequence(t *testing.T) {
	t.Parallel()

	t.Run("non_agent_profile", func(t *testing.T) {
		t.Parallel()

		mock := &vm.MockBackend{}
		e := &Engine{}
		p := &config.Profile{}

		if err := e.Run("test-profile", p, mock); err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}

		if len(mock.SSHCommandCalls) != len(expectedCommands) {
			t.Fatalf("SSHCommand call count = %d, want %d", len(mock.SSHCommandCalls), len(expectedCommands))
		}

		for i, want := range expectedCommands {
			got := mock.SSHCommandCalls[i]
			if got.Profile != "test-profile" {
				t.Errorf("call[%d].Profile = %q, want %q", i, got.Profile, "test-profile")
			}
			if got.Command != want {
				t.Errorf("call[%d].Command = %q, want %q", i, got.Command, want)
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

		// Expect all base steps plus the daemon installation step.
		wantCount := len(expectedCommands) + 1
		if len(mock.SSHCommandCalls) != wantCount {
			t.Fatalf("SSHCommand call count = %d, want %d", len(mock.SSHCommandCalls), wantCount)
		}

		// Verify daemon installation is issued as the final command.
		last := mock.SSHCommandCalls[len(mock.SSHCommandCalls)-1]
		wantDaemon := "openclaw onboard --install-daemon"
		if last.Command != wantDaemon {
			t.Errorf("final SSHCommand = %q, want %q", last.Command, wantDaemon)
		}
	})
}

// TestEngine_Run_StopsOnError verifies that Run returns an error immediately
// upon the first failed SSHCommand call and does not issue subsequent commands.
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

	// Only one call should have been issued before Run aborted.
	if len(mock.SSHCommandCalls) != 1 {
		t.Errorf("SSHCommand call count = %d, want 1 (Run must stop at first error)", len(mock.SSHCommandCalls))
	}
}

// TestEngine_DeployConfig_IsNoOp verifies that DeployConfig returns nil without
// issuing any SSH commands. The macOS provisioner relies on OpenClaw to manage
// its own runtime configuration.
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
