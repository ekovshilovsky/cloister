// Package backup provides backup, restore, and rotation logic for cloister
// session data stored inside VMs. Backups capture the ~/.claude directory tree,
// excluding directories that are either host-mounted (plugins, skills, agents)
// or ephemeral (cache, telemetry, debug, .gnupg-local).
package backup

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// maxBackups is the maximum number of backup archives retained per profile.
// When a new backup is created and the count exceeds this limit, the oldest
// archives are deleted until only maxBackups remain.
const maxBackups = 5

// remoteArchivePath is the path inside the VM where the tar archive is staged
// before being streamed to the host.
const remoteArchivePath = "/tmp/cloister-backup.tar.gz"

// excludedDirs lists the subdirectories of ~/.claude that are omitted from
// backups. Host-mounted directories (plugins, skills, agents) are excluded
// because they are managed externally and will be re-mounted after a rebuild.
// Ephemeral directories (cache, telemetry, debug) contain no persistent user
// data. .gnupg-local is excluded for security; GPG keys are managed separately.
var excludedDirs = []string{
	"plugins",
	"skills",
	"agents",
	"cache",
	"telemetry",
	"debug",
	".gnupg-local",
}

// backupDir returns the host-side directory used to store backups for the
// named profile, i.e. ~/.cloister/backups/<profile>.
func backupDir(profile string) (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving config directory: %w", err)
	}
	return filepath.Join(dir, "backups", profile), nil
}

// sshCatCommand builds an exec.Cmd that runs `cat <remotePath>` inside the VM
// using the backend's SSH connection parameters. This preserves binary fidelity
// for streaming archive data, unlike SSHCommand which captures combined text
// output.
func sshCatCommand(backend vm.Backend, profile string, remotePath string) *exec.Cmd {
	access := backend.SSHConfig(profile)
	if access.ConfigFile != "" {
		// Lima/Colima style: use the generated SSH config file and host alias.
		return exec.Command("ssh", "-F", access.ConfigFile, access.HostAlias, "cat", remotePath)
	}
	// Direct SSH style: connect via host, user, and key file.
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
	}
	if access.KeyFile != "" {
		args = append(args, "-i", access.KeyFile)
	}
	args = append(args, fmt.Sprintf("%s@%s", access.User, access.Host), "cat", remotePath)
	return exec.Command("ssh", args...)
}

// sshStdinCommand builds an exec.Cmd that pipes data to a command inside the VM
// via stdin using the backend's SSH connection parameters. This preserves binary
// fidelity for streaming archive data into the VM.
func sshStdinCommand(backend vm.Backend, profile string, remoteCmd string) *exec.Cmd {
	access := backend.SSHConfig(profile)
	if access.ConfigFile != "" {
		// Lima/Colima style: use the generated SSH config file and host alias.
		return exec.Command("ssh", "-F", access.ConfigFile, access.HostAlias, "bash", "-lc", remoteCmd)
	}
	// Direct SSH style: connect via host, user, and key file.
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
	}
	if access.KeyFile != "" {
		args = append(args, "-i", access.KeyFile)
	}
	args = append(args, fmt.Sprintf("%s@%s", access.User, access.Host), "bash", "-lc", remoteCmd)
	return exec.Command("ssh", args...)
}

// Backup creates a compressed tar archive of the ~/.claude directory inside the
// running VM identified by profile, streams it to the host, stores it at
// ~/.cloister/backups/<profile>/<timestamp>.tar.gz, and prunes old archives so
// that at most maxBackups are retained.
//
// The VM must be in the running state before calling this function. An error is
// returned if the VM is not running or if any step of the backup process fails.
//
// Returns the absolute path of the newly created backup archive.
func Backup(profile string, backend vm.Backend) (string, error) {
	if !backend.IsRunning(profile) {
		return "", fmt.Errorf("VM for profile %q is not running", profile)
	}

	// Build the tar exclusion arguments. Each excluded directory is expressed
	// relative to the ~/.claude source root so that the archive paths remain
	// portable regardless of where ~ resolves inside the VM.
	excludeArgs := ""
	for _, dir := range excludedDirs {
		excludeArgs += fmt.Sprintf(" --exclude=.claude/%s", dir)
	}

	// Create the archive inside the VM. The working directory is set to ~ so
	// that the archive contains paths relative to the home directory
	// (e.g. .claude/settings.json rather than /home/user/.claude/settings.json).
	tarCmd := fmt.Sprintf(
		"cd ~ && tar czf %s%s .claude/ 2>/dev/null || true",
		remoteArchivePath,
		excludeArgs,
	)
	if _, err := backend.SSHCommand(profile, tarCmd); err != nil {
		return "", fmt.Errorf("creating tar archive in VM: %w", err)
	}

	// Ensure the host-side backup directory exists before writing to it.
	dir, err := backupDir(profile)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating backup directory %s: %w", dir, err)
	}

	// Construct a timestamp-based filename for the new archive.
	timestamp := time.Now().UTC().Format("20060102-150405")
	localPath := filepath.Join(dir, timestamp+".tar.gz")

	// Stream the archive from the VM to the host using the backend's SSH
	// connection parameters. Using raw SSH (rather than backend.SSHCommand)
	// preserves binary fidelity because SSHCommand captures combined text
	// output which may not be safe for arbitrary binary data.
	catCmd := sshCatCommand(backend, profile, remoteArchivePath)
	data, err := catCmd.Output()
	if err != nil {
		return "", fmt.Errorf("streaming archive from VM: %w", err)
	}

	if err := os.WriteFile(localPath, data, 0o600); err != nil {
		return "", fmt.Errorf("writing backup archive to %s: %w", localPath, err)
	}

	// Remove the temporary archive from the VM to reclaim disk space.
	_, _ = backend.SSHCommand(profile, fmt.Sprintf("rm -f %s", remoteArchivePath))

	// Rotate old archives, keeping only the most recent maxBackups files.
	if err := pruneBackups(dir); err != nil {
		// Pruning failure is non-fatal; the backup itself succeeded.
		fmt.Fprintf(os.Stderr, "warning: pruning old backups: %v\n", err)
	}

	return localPath, nil
}

// Restore extracts a backup archive into the home directory of the running VM
// identified by profile. The --skip-old-files flag prevents overwriting files
// that already exist in the VM, so only absent files are restored.
//
// The VM must be in the running state before calling this function.
func Restore(profile string, backupPath string, backend vm.Backend) error {
	if !backend.IsRunning(profile) {
		return fmt.Errorf("VM for profile %q is not running", profile)
	}

	// Read the archive from disk and stream it into the VM via stdin. The
	// archive is piped to tar running inside the VM so that no intermediate
	// copy needs to be staged on the VM's filesystem.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("reading backup archive %s: %w", backupPath, err)
	}

	// Stage the archive on the VM at the well-known temporary path so that
	// tar can read it using a simple file argument rather than stdin redirection,
	// which avoids shell quoting complexities in the SSHCommand wrapper.
	stageCmd := sshStdinCommand(backend, profile, fmt.Sprintf("cat > %s", remoteArchivePath))
	stageCmd.Stdin = bytes.NewReader(data)
	if out, err := stageCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("staging archive in VM: %w\n%s", err, out)
	}

	// Extract the archive into the home directory. --skip-old-files ensures
	// that files already present in the VM are not overwritten, making the
	// restore operation safe to run on a partially provisioned VM.
	extractCmd := "cd ~ && tar xzf " + remoteArchivePath + " --skip-old-files 2>/dev/null; rm -f " + remoteArchivePath
	if _, err := backend.SSHCommand(profile, extractCmd); err != nil {
		return fmt.Errorf("extracting backup archive in VM: %w", err)
	}

	return nil
}

// LatestBackup returns the absolute path of the most recent backup archive for
// the named profile. An error is returned if no backups exist.
func LatestBackup(profile string) (string, error) {
	files, err := ListBackups(profile)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no backups found for profile %q", profile)
	}
	dir, err := backupDir(profile)
	if err != nil {
		return "", err
	}
	// ListBackups returns filenames sorted oldest to newest; the last entry is
	// the most recent backup.
	return filepath.Join(dir, files[len(files)-1]), nil
}

// ListBackups returns all backup archive filenames for the named profile,
// sorted from oldest to newest by filename (which is timestamp-based and
// therefore lexicographically ordered). Only the filenames are returned, not
// full paths.
func ListBackups(profile string) ([]string, error) {
	dir, err := backupDir(profile)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing backups in %s: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}

	// Sort lexicographically. Because filenames use the YYYYMMDD-HHMMSS
	// timestamp format, lexicographic order is equivalent to chronological order.
	sort.Strings(names)
	return names, nil
}

// pruneBackups removes the oldest backup archives from dir until at most
// maxBackups files remain. Files are sorted lexicographically (oldest first)
// so the leading entries are the candidates for deletion.
func pruneBackups(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading backup directory: %w", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	// Delete files from the beginning of the sorted slice until the count is
	// within the allowed maximum.
	for len(names) > maxBackups {
		target := filepath.Join(dir, names[0])
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("removing old backup %s: %w", target, err)
		}
		names = names[1:]
	}

	return nil
}
