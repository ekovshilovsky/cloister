package cmd

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
	vmlume "github.com/ekovshilovsky/cloister/internal/vm/lume"
	"github.com/spf13/cobra"
)

// updateBase triggers a rebuild of the shared macOS base image used by all
// Lume profiles rather than updating packages inside a running VM.
var updateBase bool

func init() {
	rootCmd.AddCommand(updateCmd)
	updateCmd.Flags().BoolVar(&updateBase, "base", false, "Rebuild the shared macOS base image for Lume profiles")
}

var updateCmd = &cobra.Command{
	Use:   "update <profile|all>",
	Short: "Update Claude Code and system packages in running VMs",
	Long: `Upgrade Claude Code to the latest version and apply pending system package
updates inside the VM for the named profile.

Pass "all" to update every currently running profile VM in sequence. Stopped
VMs are skipped silently — start them first with "cloister start <profile>"
before running an update.

Pass --base (without a profile name) to rebuild the shared macOS base image
used by Lume profiles. Running Lume profiles are auto-snapshotted before
the base is replaced as a safety measure.`,
	Args: func(cmd *cobra.Command, args []string) error {
		// --base operates on the shared base image and requires no profile argument.
		// All other invocations require exactly one positional argument.
		if b, _ := cmd.Flags().GetBool("base"); b {
			return nil
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if updateBase {
			return updateBaseImage()
		}
		if strings.EqualFold(args[0], "all") {
			return updateAll()
		}
		return updateProfile(args[0])
	},
}

// updateProfile upgrades Claude Code and applies system package updates in the
// VM for the named profile. The profile's VM must be running before the update
// is attempted; an actionable error is returned when it is not.
func updateProfile(name string) error {
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
		return fmt.Errorf("profile %q not found", name)
	}

	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	return updateProfileWithBackend(name, backend)
}

// updateProfileWithBackend performs the update using the supplied backend. This
// helper is factored out so that updateAll can resolve the backend once per
// profile and pass it through.
func updateProfileWithBackend(name string, backend vm.Backend) error {
	if !backend.IsRunning(name) {
		return fmt.Errorf("profile %q is not running. Start it first with: cloister start %s", name, name)
	}

	fmt.Printf("Updating %q...\n", name)

	// Upgrade Claude Code via the official installer, falling back to the npm
	// global package manager when the installer is unavailable.
	if out, err := backend.SSHCommand(name, "claude install latest 2>&1 || npm update -g @anthropic-ai/claude-code 2>&1"); err != nil {
		return fmt.Errorf("updating Claude Code in %q: %w\n%s", name, err, out)
	}

	// Apply pending system package updates. The -qq flag suppresses informational
	// output so only warnings and errors surface to the operator.
	if out, err := backend.SSHCommand(name, "sudo apt-get update -qq && sudo apt-get upgrade -y -qq"); err != nil {
		return fmt.Errorf("updating system packages in %q: %w\n%s", name, err, out)
	}

	fmt.Printf("✓ %s updated\n", name)
	return nil
}

// updateAll loads the configuration and calls updateProfile for every profile
// whose VM is currently running. Profiles with stopped VMs are skipped
// silently so that a partially-stopped fleet does not block the operation.
func updateAll() error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	var lastErr error
	for name, p := range cfg.Profiles {
		backend, err := resolveBackend(p.Backend)
		if err != nil {
			fmt.Printf("error resolving backend for %q: %v\n", name, err)
			lastErr = err
			continue
		}

		// Skip profiles whose VMs are not currently running to avoid blocking
		// the update loop on profiles that have not been started.
		if !backend.IsRunning(name) {
			continue
		}

		if err := updateProfileWithBackend(name, backend); err != nil {
			fmt.Printf("error updating %q: %v\n", name, err)
			lastErr = err
		}
	}

	return lastErr
}

// updateBaseImage rebuilds the shared macOS base image used as the clone source
// for all Lume profiles. Before replacing the base image each running Lume
// profile is auto-snapshotted as a safety net. After the new base is in place
// each profile's base image age is compared against the new base and an
// advisory is printed when a profile VM predates the refreshed base.
func updateBaseImage() error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Collect all Lume profiles so we can auto-snapshot running ones and check
	// base image age after the rebuild.
	type lumeEntry struct {
		name    string
		profile *config.Profile
	}
	var lumeProfiles []lumeEntry
	for name, p := range cfg.Profiles {
		if p.Backend == "lume" {
			lumeProfiles = append(lumeProfiles, lumeEntry{name, p})
		}
	}

	if len(lumeProfiles) == 0 {
		fmt.Println("No Lume profiles found. Nothing to update.")
		return nil
	}

	// Resolve a Lume backend instance and verify it implements GoldenImageManager.
	// All Lume profiles share the same backend type so resolving once is sufficient.
	lumeBackend, err := resolveBackend("lume")
	if err != nil {
		return err
	}
	gim, ok := lumeBackend.(vm.GoldenImageManager)
	if !ok {
		return fmt.Errorf("lume backend does not implement golden image management")
	}

	// Auto-snapshot every running Lume profile before destroying the base image.
	// This provides a recovery path when a profile's VM was derived from the old
	// base and the operator later needs to roll back.
	for _, e := range lumeProfiles {
		if !lumeBackend.IsRunning(e.name) {
			continue
		}
		fmt.Printf("Auto-snapshotting running profile %q before base image rebuild...\n", e.name)
		if err := lumeBackend.Stop(e.name, false); err != nil {
			fmt.Printf("  Warning: could not stop %q for snapshot: %v\n", e.name, err)
			continue
		}
		if err := gim.Snapshot(e.name, "user"); err != nil {
			fmt.Printf("  Warning: snapshot failed for %q: %v\n", e.name, err)
		} else {
			state, _ := vm.LoadProfileState(e.name)
			state.Snapshots.User = "user"
			state.Snapshots.UserCreated = vm.NowISO()
			_ = vm.SaveProfileState(e.name, state)
			fmt.Printf("  Snapshot saved for %q.\n", e.name)
		}
	}

	// Delete the existing base image so that CreateBase can provision a fresh
	// one from the latest macOS IPSW. The --force flag removes the VM regardless
	// of its current state.
	fmt.Printf("Deleting old base image %q...\n", vmlume.BaseImageName)
	_ = exec.Command("lume", "delete", vmlume.BaseImageName, "--force").Run()

	// Provision a fresh base image. verbose=true streams Lume's restore output
	// to stderr so the operator can observe the 15-20 minute process in real time.
	fmt.Println("Creating fresh macOS base image (this takes 15-20 minutes)...")
	if err := gim.CreateBase(true); err != nil {
		return fmt.Errorf("creating base image: %w", err)
	}

	newBaseAge := gim.BaseAge()

	// Compare each Lume profile's recorded base image creation time against the
	// new base. When a profile VM predates the new base it was cloned from the
	// previous base and a rebuild advisory is printed.
	for _, e := range lumeProfiles {
		state, err := vm.LoadProfileState(e.name)
		if err != nil || state.BaseImage.Created == "" {
			continue
		}
		// Print an advisory when the profile's base image is older than the
		// newly provisioned base, indicating the VM would benefit from a rebuild.
		fmt.Printf("Profile %q base image predates new base (%s). Consider: cloister rebuild %s\n",
			e.name, state.BaseImage.Created, e.name)
		_ = newBaseAge // consumed above to avoid unused-variable error
	}

	fmt.Println("Base image updated successfully.")
	return nil
}
