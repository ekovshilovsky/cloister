package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/memory"
	"github.com/ekovshilovsky/cloister/internal/terminal"
	"github.com/ekovshilovsky/cloister/internal/tunnel"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// enterProfile is the primary user interaction for cloister. It starts the VM
// for the named profile if it is not already running, records the entry
// timestamp for idle-time tracking, and then drops the user into an interactive
// SSH session inside the VM.
func enterProfile(name string) error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	p, ok := cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("profile %q not found. Create it with: cloister create %s", name, name)
	}

	// Ensure any zero-value resource fields are filled in with package defaults
	// before they are passed to the VM layer.
	p.ApplyDefaults()

	if !vm.IsRunning(name) {
		// Build a map of currently running profiles so the memory budget check
		// can compute current total consumption before starting the new VM.
		vms, _ := vm.List(false)
		running := make(map[string]bool)
		for _, v := range vms {
			if v.Status == "Running" {
				running[vm.ProfileFromVMName(v.Name)] = true
			}
		}

		// Evaluate whether starting this profile would exceed the configured
		// memory budget. When exceeded, present the user with an eviction
		// suggestion and prompt for confirmation before proceeding.
		result := memory.CheckDefault(cfg, name, running)
		if result.Exceeded {
			fmt.Print(result.FormatWarning())
			fmt.Print(result.FormatSuggestion())
			reader := bufio.NewReader(os.Stdin)
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(strings.ToLower(answer))
			if answer == "" || answer == "y" {
				// Stop the longest-idle VM to reclaim enough memory.
				candidate := result.Candidates[0]
				vm.Stop(candidate.Name, false)
			} else {
				return fmt.Errorf("aborted: memory budget exceeded")
			}
		}

		fmt.Printf("Starting %q...\n", name)

		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}

		workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
		if err != nil {
			return fmt.Errorf("invalid workspace directory in profile %q: %w", name, err)
		}
		mounts := vm.BuildMounts(home, workspaceDir, p.Stacks, p.MountPolicy, p.Headless)

		if err := vm.Start(name, p.CPU, p.Memory, p.Disk, mounts, false); err != nil {
			return fmt.Errorf("starting VM for profile %q: %w", name, err)
		}
	}

	// Probe host services and apply the profile's tunnel consent policy to
	// determine which services are forwarded into the VM.
	results := tunnel.Discover()
	resolvedPolicy := p.TunnelPolicy.ResolveForTunnels(p.Headless)
	results = tunnel.FilterByPolicy(results, resolvedPolicy)
	tunnel.PrintDiscovery(results)
	if err := tunnel.StartAll(name, results, cfg.Tunnels); err != nil {
		// Tunnel failures are non-fatal: the user can still enter the VM
		// without forwarded services.
		fmt.Fprintf(os.Stderr, "warning: tunnel setup incomplete: %v\n", err)
	}

	// Deploy authentication tokens for tunneled services that require them
	// (e.g., op-forward needs a refresh token to authenticate with the host daemon).
	if err := tunnel.DeployShims(name, results); err != nil {
		fmt.Fprintf(os.Stderr, "warning: shim deployment incomplete: %v\n", err)
	}

	// Apply terminal visual identity: accent color and window/tab titles on
	// iTerm2, or a plain-text banner on other terminal emulators.
	terminal.SetIdentity(name, p.Color)

	// Record the current Unix timestamp so that the status command can
	// calculate how long ago this profile was last entered.
	if err := writeLastEntryTimestamp(name); err != nil {
		// Non-fatal: idle tracking is best-effort and should not block entry.
		fmt.Fprintf(os.Stderr, "warning: could not record entry timestamp: %v\n", err)
	}

	fmt.Printf("Entering %s...\n", name)
	return vm.SSH(name)
}

// writeLastEntryTimestamp persists the current Unix timestamp to
// ~/.cloister/state/<profile>.last_entry. The file is used by the status
// command to compute the idle duration for each profile.
func writeLastEntryTimestamp(profile string) error {
	dir, err := config.ConfigDir()
	if err != nil {
		return fmt.Errorf("resolving config dir: %w", err)
	}

	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path := filepath.Join(stateDir, profile+".last_entry")
	return os.WriteFile(path, []byte(ts), 0o600)
}
