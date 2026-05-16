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
	vmcolima "github.com/ekovshilovsky/cloister/internal/vm/colima"
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

	if p.Headless {
		return fmt.Errorf("%q is a headless agent profile. Use 'cloister agent %s' to manage it", name, name)
	}

	// Ensure any zero-value resource fields are filled in with package defaults
	// before they are passed to the VM layer.
	p.ApplyDefaults()

	// Resolve the backend for this profile so that all VM operations use the
	// correct hypervisor implementation.
	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	// Lume profiles are headless-only in this release. Interactive SSH entry
	// is not supported through cloister; the user must connect directly via
	// the standard SSH client using the provisioned key pair and mDNS hostname.
	if p.Backend == "lume" {
		fmt.Printf("Profile %q is a headless Lume profile.\n", name)
		fmt.Printf("Use 'cloister agent' subcommands to manage it.\n\n")
		fmt.Printf("For SSH access:\n")
		fmt.Printf("  ssh -i ~/.cloister/keys/cloister-%s lume@cloister-%s.local\n", name, name)
		return nil
	}

	// Surface root-disk drift between cloister's config and the materialised
	// Colima/Lima state. Colima honors --root-disk only at VM creation; an
	// existing VM created before the field existed (or with a smaller
	// root_disk in config.yaml) keeps its original size on every subsequent
	// start, even though cloister's defaults now request a larger one.
	if actual, err := vmcolima.RootDiskGB(name); err == nil && actual > 0 && actual < p.RootDisk {
		fmt.Fprintf(os.Stderr, "warning: profile %q root disk is %d GiB but config wants %d GiB.\n", name, actual, p.RootDisk)
		fmt.Fprintf(os.Stderr, "  run 'cloister resize %s' to grow it (requires VM stop).\n\n", name)
	}

	if !backend.IsRunning(name) {
		// Disk-size drift reconciliation runs before any backend.Start so we
		// catch shrink-would-fail cases ("disk shrinking is not supported")
		// and grow-would-happen cases before colima sees flag values it
		// cannot honor. The user is prompted interactively per drift; a
		// non-TTY invocation returns a descriptive error rather than
		// guessing silently. See cmd/disk_reconcile.go for the full rules.
		if p.Backend != "lume" {
			if err := reconcileDiskDrift(os.Stderr, cfgPath, cfg, name, p); err != nil {
				return err
			}
		}

		// Build a map of currently running profiles so the memory budget check
		// can compute current total consumption before starting the new VM.
		vms, _ := backend.List(false)
		running := make(map[string]bool)
		for _, v := range vms {
			if v.Status == "Running" {
				running[backend.ProfileFromVMName(v.Name)] = true
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
				backend.Stop(candidate.Name, false)
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

		if err := backend.Start(name, p.CPU, p.Memory, p.Disk, p.RootDisk, p.MountInotify, mounts, false); err != nil {
			return fmt.Errorf("starting VM for profile %q: %w", name, err)
		}
	}

	// Probe host services and apply the profile's tunnel consent policy to
	// determine which services are forwarded into the VM. Profile-aware
	// discovery omits feature-gated builtins (e.g. gpg-forward when
	// GPGSigning is unset) so the user does not see noise for services they
	// have not opted into.
	results := tunnel.DiscoverForProfile(p)
	resolvedPolicy := p.TunnelPolicy.ResolveForTunnels(p.Headless)
	results = tunnel.FilterByPolicy(results, resolvedPolicy)
	tunnel.PrintDiscovery(results)
	if err := tunnel.StartAll(name, backend, results, cfg.Tunnels); err != nil {
		// Tunnel failures are non-fatal: the user can still enter the VM
		// without forwarded services.
		fmt.Fprintf(os.Stderr, "warning: tunnel setup incomplete: %v\n", err)
	}

	// Deploy authentication tokens for tunneled services that require them
	// (e.g., op-forward needs a refresh token to authenticate with the host daemon).
	if err := tunnel.DeployShims(name, backend, results); err != nil {
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
	sshErr := backend.SSH(name)

	// Defensively reset DEC private modes that an in-VM tool may have left
	// enabled when the session exited non-cleanly. Modern terminal emulators
	// (iTerm2, WezTerm, Ghostty) latch these modes until they receive the
	// matching disable sequence:
	//
	//   1004 — focus reporting; when leaked, the host shell receives "ESC [ I"
	//          / "ESC [ O" on every focus change and prints them as garbage,
	//          and iTerm surfaces a "focus reporting was left on" prompt.
	//   2004 — bracketed paste; when leaked, paste behavior in the host
	//          shell can break or insert spurious framing characters.
	//
	// These resets are idempotent (cleanly-exited sessions will already have
	// sent them, and the local shell's readline re-enables 2004 on its next
	// prompt redraw if configured to use it), so issuing them unconditionally
	// is safe.
	fmt.Print("\x1b[?1004l\x1b[?2004l")

	return sshErr
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
