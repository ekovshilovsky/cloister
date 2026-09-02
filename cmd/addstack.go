package cmd

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloister.io/internal/config"
	"cloister.io/internal/profile"
	"cloister.io/internal/provision"
	"cloister.io/internal/tunnel"
	"cloister.io/internal/vm"
	"github.com/spf13/cobra"
)

var addStackYes bool

func init() {
	rootCmd.AddCommand(addStackCmd)
	addStackCmd.Flags().BoolVarP(&addStackYes, "yes", "y", false, "Skip confirmation prompts and proceed automatically")
}

var addStackCmd = &cobra.Command{
	Use:   "add-stack <profile> <stack>",
	Short: "Add a provisioning stack to an existing profile",
	Args:  cobra.ExactArgs(2),
	RunE:  runAddStack,
}

// runAddStack installs a named toolchain stack into the VM for an existing
// profile, updating the persisted configuration so the stack is retained
// across rebuilds.
func runAddStack(cmd *cobra.Command, args []string) error {
	profileName := args[0]
	stackName := args[1]

	// Validate the requested stack name against the set of supported stacks
	// before performing any I/O or VM operations.
	if err := profile.ValidateStacks([]string{stackName}); err != nil {
		return err
	}

	cfgPath, err := config.ConfigPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	p, ok := cfg.Profiles[profileName]
	if !ok {
		return fmt.Errorf("profile %q not found", profileName)
	}

	// Resolve the backend for this profile so that all VM operations use the
	// correct hypervisor implementation.
	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	// Prevent duplicate stack entries to avoid running the provisioning script
	// more than once for the same stack.
	for _, s := range p.Stacks {
		if s == stackName {
			return fmt.Errorf("stack %q is already installed in %q", stackName, profileName)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	// Compute the mount set with the current stacks so we can detect whether
	// adding the new stack introduces any additional host directory bindings.
	mountsBefore := vm.BuildSupplementalMounts(home, p.Stacks, p.MountPolicy, p.Headless)

	// Compute the mount set that would apply once the new stack is included.
	stacksAfter := append(append([]string(nil), p.Stacks...), stackName)
	mountsAfter := vm.BuildSupplementalMounts(home, stacksAfter, p.MountPolicy, p.Headless)

	// Detect any change in the supplemental mount contract.
	mountsChanged := vm.MountsChanged(mountsBefore, mountsAfter)

	if mountsChanged && backend.IsRunning(profileName) {
		if !addStackYes {
			// Warn the user that the new mount cannot be activated without
			// restarting the VM and offer to perform the restart immediately.
			reader := bufio.NewReader(os.Stdin)
			fmt.Printf("Adding %s stack requires a VM restart to update mounts. Restart now? [Y/n]: ", stackName)
			answer, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("reading input: %w", err)
			}
			answer = strings.TrimSpace(answer)

			if answer != "" && !strings.EqualFold(answer, "y") {
				// User declined the restart. Persist the stack configuration and
				// instruct them to apply mount changes via a full rebuild.
				p.Stacks = append(p.Stacks, stackName)
				if err := config.Save(cfgPath, cfg); err != nil {
					return err
				}
				fmt.Println("Config saved. Run 'cloister rebuild <profile>' to apply mount changes.")
				return nil
			}
		}

		// User accepted the restart. Tear down SSH tunnels first so stale
		// forward processes from the old VM instance do not hold pinned host
		// ports, then stop the VM, start it with the updated mount set, and
		// proceed to provisioning.
		fmt.Printf("Stopping %q...\n", profileName)
		if err := stopVM(backend, profileName, p, false, false); err != nil {
			return fmt.Errorf("stopping VM: %w", err)
		}
		tunnel.StopAll(profileName)

		fmt.Printf("Starting %q with updated mounts...\n", profileName)
		updated := *p
		updated.Stacks = stacksAfter
		if err := startVM(backend, profileName, &updated, nil, false); err != nil {
			return fmt.Errorf("starting VM: %w", err)
		}
	} else if !backend.IsRunning(profileName) {
		// VM is not running; start it with the post-stack mount set so that
		// any mount additions introduced by the new stack are applied now.
		fmt.Printf("Starting %q...\n", profileName)
		updated := *p
		updated.Stacks = stacksAfter
		if err := startVM(backend, profileName, &updated, nil, false); err != nil {
			return fmt.Errorf("failed to start: %w", err)
		}
	}

	// When the tunnel policy is an explicit allowlist, automatically include
	// the stack's tunnel name so the service is reachable without manual edits.
	if shouldAutoAddTunnel(stackName, p.TunnelPolicy) {
		p.TunnelPolicy.Names = append(p.TunnelPolicy.Names, "ollama")
		fmt.Println("Added 'ollama' to tunnel whitelist")
	}

	// Read the embedded stack provisioning script and pipe it to bash inside
	// the VM via stdin to avoid shell quoting issues with multi-line scripts.
	// The guest output goes to the run log; the console carries progress.
	scriptPath := fmt.Sprintf("scripts/stack-%s.sh", stackName)
	scriptData, err := provision.Scripts.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("reading stack script: %w", err)
	}

	session := startProvisionSession(profileName, "add-stack")
	defer session.Close()

	step := session.Step(stackName + " stack")
	if _, err := backend.SSHScriptTo(profileName, string(scriptData), step.Writer()); err != nil {
		step.Fail()
		return fmt.Errorf("stack installation failed: %w", err)
	}
	step.Done()

	// Append the stack to the profile's Stacks list and persist the updated
	// configuration so subsequent commands (e.g. rebuild) reproduce the same
	// set of stacks.
	p.Stacks = append(p.Stacks, stackName)
	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}

	if path := session.LogPath(); path != "" {
		fmt.Printf("Stack %q added to %q.  Log: %s\n", stackName, profileName, path)
	} else {
		fmt.Printf("Stack %q added to %q\n", stackName, profileName)
	}

	// After provisioning the ollama stack, probe the host Ollama service and
	// report whether the model store directory is available.
	if stackName == "ollama" {
		printOllamaHostGuidance(home)
	}

	if stackName == "agentgrid" {
		if err := tunnel.StartStackLocalForwards(cfgPath, cfg, profileName, backend); err != nil {
			fmt.Fprintf(os.Stderr, "warning: agentgrid local forward: %v\n", err)
		}
		printAgentGridGuidance(profileName, backend.VMName(profileName), p.LocalForwardPorts["agentgrid"])
	}

	return nil
}

// shouldAutoAddTunnel reports whether the given stack should trigger automatic
// addition of its tunnel name to an explicit tunnel policy allowlist. This
// applies only to the ollama stack when an allowlist is already configured and
// does not already include the ollama tunnel, preventing redundant entries.
func shouldAutoAddTunnel(stackName string, policy config.ResourcePolicy) bool {
	if stackName != "ollama" {
		return false
	}
	return policy.IsSet && len(policy.Names) > 0 && !policy.IsAllowed("ollama")
}

// printOllamaHostGuidance probes the host Ollama API and reports its
// availability, then checks whether the model store directory exists.
// This guides the user in setting up Ollama on the host so that models
// mounted into the VM are accessible without redundant downloads.
func printOllamaHostGuidance(homeDir string) {
	const ollamaPort = "11434"
	const probeTimeout = 500 * time.Millisecond

	conn, err := net.DialTimeout("tcp", "localhost:"+ollamaPort, probeTimeout)
	if err != nil {
		fmt.Println()
		fmt.Println("Note: Ollama is not running on this host (localhost:11434 unreachable).")
		fmt.Println("Install Ollama from https://ollama.com/download to enable model sharing")
		fmt.Println("between the host and the VM without re-downloading models.")
	} else {
		conn.Close()
		fmt.Println()
		fmt.Println("Host Ollama detected on localhost:11434.")
	}

	ollamaModels := filepath.Join(homeDir, ".ollama", "models")
	if _, err := os.Stat(ollamaModels); os.IsNotExist(err) {
		fmt.Printf("Note: %s does not exist yet.\n", ollamaModels)
		fmt.Println("Pull at least one model on the host ('ollama pull <model>') to populate")
		fmt.Println("the model store so it can be mounted read-only into the VM.")
	} else {
		fmt.Printf("Host model store found at %s\n", ollamaModels)
	}
}

// printAgentGridGuidance prints connection instructions for the Agent Grid
// headless daemon. The host port is read from the live forward record rather
// than assumed: it differs from the guest port (8765) to avoid colliding with
// the Mac desktop app's own daemon, and may shift further if the preferred
// host port was itself taken. When no forward is live, the profile's pinned
// port (pinnedPort, 0 when unset) is the endpoint clients will actually get,
// so it takes precedence over the preferred-port constant.
func printAgentGridGuidance(profileName, pairLabel string, pinnedPort int) {
	hostPort, ok := tunnel.LocalForwardPort(profileName, "agentgrid")
	if !ok {
		hostPort = pinnedPort
		if hostPort == 0 {
			hostPort = tunnel.AgentGridHostPort
		}
	}

	fmt.Println()
	fmt.Println("Agent Grid headless daemon is installed in the VM.")
	fmt.Printf("Reachable on your Mac at 127.0.0.1:%d (forwards to VM :%d).\n", hostPort, tunnel.AgentGridDaemonPort)
	fmt.Printf("The Mac app keeps its own daemon on :%d, so cloister uses :%d instead.\n", tunnel.AgentGridDaemonPort, hostPort)
	fmt.Println()
	fmt.Println("Mint a pairing code inside the VM:")
	fmt.Printf("  cloister exec %s -- agent-grid-pair --label %s\n", profileName, pairLabel)
	fmt.Println()
	fmt.Println("On your Mac Agent Grid client:")
	fmt.Println("  Settings → Devices → Connect to a device")
	fmt.Printf("  Host: 127.0.0.1:%d    Code: <the 6-char code>\n", hostPort)
}
