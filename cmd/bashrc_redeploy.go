package cmd

import (
	"fmt"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/provision"
	"github.com/ekovshilovsky/cloister/internal/tunnel"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(bashrcRedeployCmd)
}

// bashrcRedeployCmd exposes DeployBashrc as a first-class command so users
// can refresh the cloister-managed ~/.bashrc inside a running VM after a
// cloister release ships an updated template (new PATH entries, new
// sourced fragments, etc.) without going through a heavier path like
// `cloister update-config <profile> --claude-cloud` (a no-op semantic
// flag that today is the only way to trigger this refresh) or
// `cloister repair <profile>` (full provisioning pipeline).
var bashrcRedeployCmd = &cobra.Command{
	Use:   "bashrc-redeploy <profile>",
	Short: "Re-render and deploy the cloister-managed ~/.bashrc into a running VM",
	Long: `Re-render the cloister-managed bashrc template against the named
profile and push it into the running VM, replacing the existing
~/.bashrc. Use this when a cloister release has shipped an updated
bashrc template — new PATH entries, new sourced fragments, etc. — and
you want those changes to take effect without rebooting the VM or
running a full repair.

The bashrc is sourced on every new login shell, so to see the new
content open a fresh shell inside the VM, or run 'source ~/.bashrc'
in an existing session.`,
	Args: cobra.ExactArgs(1),
	RunE: runBashrcRedeploy,
}

// runBashrcRedeploy looks up the profile, confirms the VM is running, and
// re-deploys both the bashrc template and the in-VM tunnel/config metadata
// that update-config emits alongside it. The two deploys are kept together
// because the in-VM cloister-vm toolkit reads from a config file whose
// contents are derived from the same Profile + tunnel registry; rendering
// only one of them risks the two going out of sync until the next full
// provision.
func runBashrcRedeploy(cmd *cobra.Command, args []string) error {
	name := args[0]

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
	if !backend.IsRunning(name) {
		return fmt.Errorf("profile %q is not running; start it first with: cloister %s", name, name)
	}

	cmd.Printf("Redeploying bashrc for %q...\n", name)
	if err := provision.DeployBashrc(name, p); err != nil {
		return fmt.Errorf("redeploying bashrc: %w", err)
	}
	if err := provision.DeployVMConfig(name, p, tunnel.BuiltinTunnelDefs(), provision.ResolveStartDir(p.StartDir)); err != nil {
		// VM-config deployment failure is non-fatal: the bashrc update
		// has already landed and is the primary purpose of this command.
		// The in-VM toolkit will fall back to its previous config until
		// the next successful deploy.
		fmt.Printf("Warning: deploying VM config: %v\n", err)
	}
	cmd.Println("Done. Open a new shell in the VM (or run 'source ~/.bashrc') for changes to take effect.")
	return nil
}
