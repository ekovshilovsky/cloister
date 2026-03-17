package cmd

import (
	"fmt"

	"github.com/ekovshilovsky/cloister/internal/backup"
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(backupCmd)
}

var backupCmd = &cobra.Command{
	Use:   "backup [profile|all]",
	Short: "Back up session data from a running VM",
	Long: `Create a compressed archive of the ~/.claude directory inside the named
VM and store it on the host at ~/.cloister/backups/<profile>/<timestamp>.tar.gz.

Pass "all" to back up every currently running profile in sequence. Only the
five most recent archives are kept per profile; older ones are deleted automatically.`,
	Args: cobra.ExactArgs(1),
	RunE: runBackup,
}

// runBackup is the handler for the backup subcommand.
func runBackup(cmd *cobra.Command, args []string) error {
	target := args[0]

	if target == "all" {
		return runBackupAll(cmd)
	}

	return runBackupOne(cmd, target)
}

// runBackupOne creates a single backup for the named profile.
func runBackupOne(cmd *cobra.Command, profile string) error {
	cmd.Printf("Backing up profile %q...\n", profile)

	path, err := backup.Backup(profile)
	if err != nil {
		return fmt.Errorf("backup %q: %w", profile, err)
	}

	cmd.Printf("Backup saved: %s\n", path)
	return nil
}

// runBackupAll iterates over every profile defined in the configuration and
// backs up each one that is currently running. Profiles that are stopped are
// skipped with an informational message.
func runBackupAll(cmd *cobra.Command) error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if len(cfg.Profiles) == 0 {
		cmd.Println("No profiles defined.")
		return nil
	}

	var lastErr error
	for name := range cfg.Profiles {
		if !vm.IsRunning(name) {
			cmd.Printf("Skipping %q (not running)\n", name)
			continue
		}
		if err := runBackupOne(cmd, name); err != nil {
			// Record the error but continue so that other profiles are not
			// skipped due to one failing VM.
			fmt.Fprintf(cmd.ErrOrStderr(), "error: %v\n", err)
			lastErr = err
		}
	}

	return lastErr
}
