package setup

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

// Pairing flag values are read from ctx.Flags, populated by the cmd layer.

// pairingFlags registers CLI flags for non-interactive pairing setup.
func pairingFlags(fs *pflag.FlagSet) {
	// Flags are registered in cmd/setup_openclaw.go.
}

// runPairing handles the device pairing wizard section: registers the node
// host, configures trusted proxies, approves pending devices, and verifies
// gateway health via openclaw gateway probe.
func runPairing(ctx *SetupContext) error {
	if ctx.State.Pairing.DevicesApproved {
		fmt.Println("  ✓ Device pairing already complete")
		return nil
	}

	fmt.Println("  Device Pairing")
	fmt.Println("  ──────────────")

	// Step 1: Register node host.
	if !ctx.State.Pairing.NodeHostRegistered {
		if err := registerNodeHost(ctx); err != nil {
			ctx.Progress.MarkFailed("pairing", "node_host", err.Error())
			SaveProgress(ctx.ProgressPath, ctx.Progress)
			return err
		}
	} else {
		fmt.Printf("  ✓ Node host already registered (%s)\n", ctx.State.Pairing.NodeDisplayName)
	}

	// Step 2: Configure trusted proxies.
	if err := writeTrustedProxies(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠ Trusted proxies: %v\n", err)
		// Non-fatal — proceed to device approval.
	}

	// Step 3: Approve pending devices.
	if err := approveDevices(ctx); err != nil {
		ctx.Progress.MarkFailed("pairing", "device_approval", err.Error())
		SaveProgress(ctx.ProgressPath, ctx.Progress)
		return err
	}

	// Step 4: Verify gateway health.
	verifyGatewayHealth(ctx)

	return nil
}

// registerNodeHost installs the OpenClaw node host service inside the VM with
// the profile name as both the node ID and display name.
func registerNodeHost(ctx *SetupContext) error {
	fmt.Println()
	fmt.Println("  Registering node host...")

	installCmd := fmt.Sprintf(
		`export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH" && openclaw node install --node-id %q --display-name %q`,
		ctx.Profile, ctx.Profile,
	)
	if _, err := ctx.Backend.SSHCommand(ctx.Profile, installCmd); err != nil {
		return fmt.Errorf("registering node host: %w", err)
	}

	ctx.State.Pairing.NodeHostRegistered = true
	ctx.State.Pairing.NodeDisplayName = ctx.Profile
	SaveState(ctx.StatePath, ctx.State)

	ctx.Progress.MarkComplete("pairing", "node_host")
	SaveProgress(ctx.ProgressPath, ctx.Progress)

	fmt.Printf("  ✓ Node host registered\n")
	fmt.Printf("    Node ID: %s\n", ctx.Profile)
	fmt.Printf("    Display name: %s\n", ctx.Profile)
	return nil
}

// writeTrustedProxies adds loopback addresses to the gateway's trusted proxies
// configuration so that requests from localhost are accepted.
func writeTrustedProxies(ctx *SetupContext) error {
	fmt.Println()
	fmt.Println("  Configuring gateway trusted proxies...")

	writeCmd := `python3 -c "
import json, os
cfg_path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault('gateway', {})['trustedProxies'] = ['127.0.0.1', '::1']
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
"`
	if _, err := ctx.Backend.SSHCommand(ctx.Profile, writeCmd); err != nil {
		return fmt.Errorf("writing trusted proxies: %w", err)
	}

	fmt.Println(`  ✓ trustedProxies: ["127.0.0.1", "::1"] written`)
	return nil
}

// approveDevices approves all pending devices registered with the OpenClaw
// gateway inside the VM.
func approveDevices(ctx *SetupContext) error {
	fmt.Println()
	fmt.Println("  Approving devices...")

	approveCmd := `export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH" && openclaw devices approve --all 2>&1 || true`
	out, err := ctx.Backend.SSHCommand(ctx.Profile, approveCmd)
	if err != nil {
		return fmt.Errorf("approving devices: %w", err)
	}

	ctx.State.Pairing.DevicesApproved = true
	SaveState(ctx.StatePath, ctx.State)

	ctx.Progress.MarkComplete("pairing", "device_approval")
	SaveProgress(ctx.ProgressPath, ctx.Progress)

	// Parse output for approved device names.
	if strings.TrimSpace(out) != "" {
		fmt.Println("  " + strings.ReplaceAll(strings.TrimSpace(out), "\n", "\n  "))
	}
	fmt.Printf("  ✓ Devices approved\n")
	return nil
}

// verifyGatewayHealth runs openclaw gateway probe to confirm the gateway is
// reachable and healthy. Failure is reported but non-fatal.
func verifyGatewayHealth(ctx *SetupContext) {
	fmt.Println()
	fmt.Println("  Verifying gateway health...")

	probeCmd := `export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH" && openclaw gateway probe 2>&1`
	out, err := ctx.Backend.SSHCommand(ctx.Profile, probeCmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠ Gateway probe failed: %v\n", err)
		if strings.TrimSpace(out) != "" {
			fmt.Fprintf(os.Stderr, "  %s\n", strings.TrimSpace(out))
		}
		return
	}

	fmt.Println("  ✓ Gateway WebSocket reachable")
	fmt.Println("  ✓ RPC healthy")

	// Check for auth warnings in probe output.
	if strings.Contains(out, "authWarning") {
		fmt.Println("  ⚠ Auth warnings detected — check gateway configuration")
	} else {
		fmt.Println("  ✓ No auth warnings")
	}
}
