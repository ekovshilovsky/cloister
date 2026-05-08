package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/tunnel"
	"github.com/ekovshilovsky/cloister/internal/vm"
	vmcolima "github.com/ekovshilovsky/cloister/internal/vm/colima"
	vmlume "github.com/ekovshilovsky/cloister/internal/vm/lume"
	"github.com/spf13/cobra"
)

// deleteFlags holds flag state for the delete subcommand.
type deleteFlags struct {
	yes bool
}

var dlf deleteFlags

func init() {
	rootCmd.AddCommand(deleteCmd)
	deleteCmd.Flags().BoolVarP(&dlf.yes, "yes", "y", false, "Skip the orphan-deletion confirmation prompt (no effect when the profile is in config)")
}

var deleteCmd = &cobra.Command{
	Use:   "delete <profile>",
	Short: "Delete a cloister profile and its VM",
	Long: `Permanently destroy the VM and remove the named profile from the
cloister configuration.

All isolated data stored inside the VM is lost when it is deleted. The
host-side directories mounted into the VM (e.g. ~/Code) are not affected.

When the profile is not present in cloister's config but a backend VM
carrying the cloister namespace prefix still exists (an "orphan" — for
example, a VM left behind after a manual config edit), this command prompts
before destroying it. Pass -y/--yes to skip the orphan confirmation.`,
	Args: cobra.ExactArgs(1),
	RunE: runDelete,
}

// runDelete is the handler for the delete subcommand. It dispatches to the
// configured-profile path when the named profile exists in cloister's config,
// or the orphan-VM path otherwise.
func runDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if p, ok := cfg.Profiles[name]; ok {
		return deleteConfiguredProfile(cmd, cfgPath, cfg, name, p)
	}

	return deleteOrphanVM(cmd, name)
}

// deleteConfiguredProfile destroys the VM and removes the profile entry from
// the cloister configuration. Errors from the backend are intentionally
// ignored because the VM may never have been started; the config entry is
// still removed in that case so the user is not left with a dangling profile.
func deleteConfiguredProfile(cmd *cobra.Command, cfgPath string, cfg *config.Config, name string, p *config.Profile) error {
	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	cmd.Printf("Deleting %q (this destroys all isolated data)...\n", name)

	tunnel.StopAll(name)
	_ = backend.Delete(name, false)

	delete(cfg.Profiles, name)

	if err := config.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("saving config after delete: %w", err)
	}

	cmd.Printf("Profile %q deleted\n", name)
	return nil
}

// deleteOrphanVM destroys a backend VM that carries the cloister namespace
// prefix but is not present in the cloister config. Each registered backend
// is probed in turn; the first one reporting the VM as existing is used to
// destroy it. The user is prompted for confirmation unless -y/--yes was
// passed, because deleting an unmanaged VM is destructive and the absence
// of a config entry means the user may not realise the VM is still around.
func deleteOrphanVM(cmd *cobra.Command, name string) error {
	for _, candidate := range orphanBackends() {
		if !candidate.backend.Exists(name) {
			continue
		}
		return deleteOrphanFromBackend(cmd, name, candidate)
	}
	return fmt.Errorf("profile %q not found in config and no backend VM exists for it", name)
}

// orphanCandidate pairs a backend implementation with its display label so
// the orphan-deletion message can name the hypervisor without re-deriving it
// from the backend type.
type orphanCandidate struct {
	backend vm.Backend
	label   string
}

// orphanBackends returns the backends to probe for orphan VMs, in the order
// they should be considered. Colima is checked first because it is the
// historical default and the most common source of orphans (legacy VMs from
// before profile management was strict).
func orphanBackends() []orphanCandidate {
	return []orphanCandidate{
		{backend: &vmcolima.Backend{}, label: "colima"},
		{backend: &vmlume.Backend{}, label: "lume"},
	}
}

// deleteOrphanFromBackend prompts for confirmation (unless --yes was passed)
// and destroys the orphan VM via the supplied backend.
func deleteOrphanFromBackend(cmd *cobra.Command, name string, c orphanCandidate) error {
	cmd.Printf("Profile %q is not in cloister config, but a %s VM with the cloister\n", name, c.label)
	cmd.Printf("namespace prefix exists for it. Deleting will permanently destroy that\n")
	cmd.Printf("VM and any data stored inside.\n\n")

	if !dlf.yes {
		cmd.Print("Proceed? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		ans, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading confirmation: %w", err)
		}
		ans = strings.TrimSpace(strings.ToLower(ans))
		if ans != "y" {
			cmd.Println("Delete canceled.")
			return nil
		}
	}

	tunnel.StopAll(name)
	if err := c.backend.Delete(name, false); err != nil {
		return fmt.Errorf("deleting orphan VM: %w", err)
	}

	cmd.Printf("Orphan %s VM for %q deleted.\n", c.label, name)
	return nil
}
