package cmd

import (
	"fmt"
	"os"

	"cloister.io/internal/config"
	"cloister.io/internal/workspace"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(openCmd)
}

var runOpenPath = openPath

var openCmd = &cobra.Command{
	Use:   "open <path>",
	Short: "Resolve a project, activate its workspace, and enter its VM",
	Long: `Resolve an existing host directory to the profile whose start_dir most
specifically contains it, ensure that profile is running, and enter the project.

In broker mode, start_dir is the authorization scope and the exact canonical
path passed to open is the synchronized project root. This avoids synchronizing
a broad parent and places the guest copy at its stable ~/workspaces path.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOpenPath(args[0])
	},
}

func openPath(requested string) error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	resolution, err := workspace.ResolveProfile(requested, home, cfg.Profiles)
	if err != nil {
		return err
	}
	return enterLoadedProfile(cfgPath, cfg, resolution.Profile, resolution.Path)
}
