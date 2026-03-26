package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/backup"
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/spf13/cobra"
)

// restoreFlags holds flag state for the restore subcommand.
type restoreFlags struct {
	latest bool
}

var rf restoreFlags

func init() {
	rootCmd.AddCommand(restoreCmd)
	restoreCmd.Flags().BoolVar(&rf.latest, "latest", false, "Automatically restore the most recent backup without prompting")
}

var restoreCmd = &cobra.Command{
	Use:   "restore <profile>",
	Short: "Restore session data from a backup archive",
	Long: `Extract a previously created backup archive into the running VM for the
named profile. By default the command lists available backups and prompts for
a selection. Pass --latest to automatically choose the most recent archive.

The restore uses --skip-old-files so that files already present in the VM
are not overwritten; only absent files are added.`,
	Args: cobra.ExactArgs(1),
	RunE: runRestore,
}

// runRestore is the handler for the restore subcommand.
func runRestore(cmd *cobra.Command, args []string) error {
	profile := args[0]

	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	p, ok := cfg.Profiles[profile]
	if !ok {
		return fmt.Errorf("profile %q not found", profile)
	}

	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return err
	}

	var archivePath string

	if rf.latest {
		// Non-interactive path: resolve the newest archive automatically.
		latest, err := backup.LatestBackup(profile)
		if err != nil {
			return fmt.Errorf("finding latest backup for %q: %w", profile, err)
		}
		archivePath = latest
	} else {
		// Interactive path: display available archives and prompt the user to
		// pick one by index.
		selected, err := promptBackupSelection(cmd, profile)
		if err != nil {
			return err
		}
		archivePath = selected
	}

	cmd.Printf("Restoring %q from %s...\n", profile, archivePath)

	if err := backup.Restore(profile, archivePath, backend); err != nil {
		return fmt.Errorf("restoring %q: %w", profile, err)
	}

	cmd.Printf("Restore complete for profile %q\n", profile)
	return nil
}

// promptBackupSelection lists available backups for the given profile and
// returns the absolute path of the archive selected by the user. The most
// recent backup is presented first in the numbered list.
func promptBackupSelection(cmd *cobra.Command, profile string) (string, error) {
	files, err := backup.ListBackups(profile)
	if err != nil {
		return "", fmt.Errorf("listing backups for %q: %w", profile, err)
	}

	if len(files) == 0 {
		return "", fmt.Errorf("no backups found for profile %q", profile)
	}

	// Determine the backup directory to construct absolute paths for display.
	dir, err := backupDirPath(profile)
	if err != nil {
		return "", err
	}

	// Display in reverse order (newest first) to make the default choice obvious.
	cmd.Printf("Available backups for %q:\n", profile)
	for i := len(files) - 1; i >= 0; i-- {
		displayIdx := len(files) - i
		cmd.Printf("  [%d] %s\n", displayIdx, files[i])
	}

	cmd.Print("Select backup number [1]: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading selection: %w", err)
	}
	line = strings.TrimSpace(line)

	// Default to 1 (the most recent) when the user presses enter.
	if line == "" {
		line = "1"
	}

	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(files) {
		return "", fmt.Errorf("invalid selection %q: must be a number between 1 and %d", line, len(files))
	}

	// Convert the 1-based display index back to the files slice index.
	// Display index 1 corresponds to files[len(files)-1] (newest).
	fileIdx := len(files) - idx
	return dir + "/" + files[fileIdx], nil
}

// backupDirPath returns the host-side backup directory for the given profile.
// This mirrors the internal path used by the backup package so that the restore
// command can construct absolute paths for display and selection.
func backupDirPath(profile string) (string, error) {
	files, err := backup.ListBackups(profile)
	if err != nil || len(files) == 0 {
		return "", fmt.Errorf("no backups found for profile %q", profile)
	}

	latest, err := backup.LatestBackup(profile)
	if err != nil {
		return "", err
	}
	// Strip the filename to get the directory.
	dir := strings.TrimSuffix(latest, "/"+files[len(files)-1])
	return dir, nil
}
