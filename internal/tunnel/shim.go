package tunnel

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ekovshilovsky/cloister/internal/vm"
)

// DeployShims deploys authentication tokens and configuration for tunneled
// services into the VM. The op-forward binary and shim are installed during
// base provisioning via APT; this function handles the host-side token
// deployment that cannot be done from inside the VM.
func DeployShims(profile string, available []DiscoveryResult) error {
	for _, r := range available {
		if !r.Available || r.Blocked {
			continue
		}
		if r.Tunnel.Name == "op-forward" {
			if err := deployOpForwardToken(profile); err != nil {
				// Token deployment failure is non-fatal — the user can
				// still enter the VM and deploy the token manually.
				fmt.Printf("  Warning: op-forward token deployment: %v\n", err)
			}
		}
	}
	return nil
}

// deployOpForwardToken copies the op-forward refresh token from the host
// into the VM so that the op shim can authenticate with the host daemon.
func deployOpForwardToken(profile string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	tokenPath := filepath.Join(home, "Library", "Caches", "op-forward", "refresh.token")
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return fmt.Errorf("reading op-forward token at %s: %w", tokenPath, err)
	}

	// Create the token directory and write the token inside the VM
	script := fmt.Sprintf(
		"mkdir -p ~/.cache/op-forward && echo '%s' > ~/.cache/op-forward/refresh.token && chmod 600 ~/.cache/op-forward/refresh.token",
		string(token),
	)
	if _, err := vm.SSHCommand(profile, script); err != nil {
		return fmt.Errorf("writing token to VM: %w", err)
	}

	fmt.Println("  ✓ op-forward token deployed")
	return nil
}
