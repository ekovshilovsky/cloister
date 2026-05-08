package colima

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// rootDiskRE matches the rootDisk field in a colima.yaml at column 0. The
// captured group is the size in GiB.
var rootDiskRE = regexp.MustCompile(`(?m)^rootDisk:\s*(\d+)\s*$`)

// limaDiskRE matches Lima's top-level disk field (e.g. "disk: 20GiB"). The
// captured group is the size in GiB; Lima always emits the GiB unit suffix.
var limaDiskRE = regexp.MustCompile(`(?m)^disk:\s*(\d+)GiB\s*$`)

// userHomeDir is overridable in tests so the package can be exercised without
// touching the real home directory.
var userHomeDir = os.UserHomeDir

// colimaProfileDir returns the directory where Colima persists per-profile
// state (colima.yaml lives here).
func colimaProfileDir(profile string) (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".colima", VMName(profile)), nil
}

// limaInstanceDir returns the directory where Lima persists per-instance
// state (lima.yaml and the raw root disk image live here). Lima namespaces
// instance dirs as "colima-<colima-vm-name>".
func limaInstanceDir(profile string) (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".colima", "_lima", "colima-"+VMName(profile)), nil
}

// RootDiskGB returns the rootDisk size (in GiB) recorded in the profile's
// colima.yaml. It returns 0 when the field is absent or when the colima.yaml
// has not yet been materialised (i.e. the VM has never been started). An
// error is returned only on a genuine I/O or parse failure.
func RootDiskGB(profile string) (int, error) {
	dir, err := colimaProfileDir(profile)
	if err != nil {
		return 0, err
	}
	path := filepath.Join(dir, "colima.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	m := rootDiskRE.FindSubmatch(data)
	if m == nil {
		return 0, nil
	}
	var gb int
	if _, err := fmt.Sscanf(string(m[1]), "%d", &gb); err != nil {
		return 0, fmt.Errorf("parsing rootDisk in %s: %w", path, err)
	}
	return gb, nil
}

// ResizeRootDiskFile extends the raw root-disk image of a stopped Colima VM
// to targetGB and updates the size declarations in both colima.yaml and
// lima.yaml so subsequent starts agree with the new size. The original disk
// image is preserved as disk.bak via an APFS reflink clone (instant, zero
// additional space) until the caller invokes CleanupResizeBackup.
//
// The VM must be stopped before this is called. Shrinking is refused: the
// function returns an error when the on-disk image is already at or above
// targetGB.
func ResizeRootDiskFile(profile string, targetGB int) error {
	limaDir, err := limaInstanceDir(profile)
	if err != nil {
		return err
	}
	colimaDir, err := colimaProfileDir(profile)
	if err != nil {
		return err
	}

	diskPath := filepath.Join(limaDir, "disk")
	backupPath := filepath.Join(limaDir, "disk.bak")

	fi, err := os.Stat(diskPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", diskPath, err)
	}
	targetBytes := int64(targetGB) * 1024 * 1024 * 1024
	if fi.Size() >= targetBytes {
		return fmt.Errorf("current disk size %d bytes is already >= target %d bytes; refusing to shrink", fi.Size(), targetBytes)
	}

	if err := apfsCloneFile(diskPath, backupPath); err != nil {
		return fmt.Errorf("creating APFS-clone backup at %s: %w", backupPath, err)
	}

	if err := os.Truncate(diskPath, targetBytes); err != nil {
		return fmt.Errorf("truncating %s to %d bytes: %w", diskPath, targetBytes, err)
	}

	colimaYAML := filepath.Join(colimaDir, "colima.yaml")
	if err := replaceLineInFile(colimaYAML, rootDiskRE, fmt.Sprintf("rootDisk: %d", targetGB)); err != nil {
		return fmt.Errorf("updating %s: %w", colimaYAML, err)
	}

	limaYAML := filepath.Join(limaDir, "lima.yaml")
	if err := replaceLineInFile(limaYAML, limaDiskRE, fmt.Sprintf("disk: %dGiB", targetGB)); err != nil {
		return fmt.Errorf("updating %s: %w", limaYAML, err)
	}

	return nil
}

// CleanupResizeBackup removes the disk.bak left behind by a successful
// ResizeRootDiskFile call. It is idempotent: a missing backup is not an error.
func CleanupResizeBackup(profile string) error {
	limaDir, err := limaInstanceDir(profile)
	if err != nil {
		return err
	}
	backupPath := filepath.Join(limaDir, "disk.bak")
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// apfsCloneFile invokes /bin/cp -c to create a copy-on-write clone via the
// macOS clonefile(2) syscall. The clone shares blocks with the source until
// either copy is mutated, so it is instant and consumes no additional space
// at creation time.
func apfsCloneFile(src, dst string) error {
	cmd := exec.Command("/bin/cp", "-c", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("/bin/cp -c %s %s: %w (output: %s)", src, dst, err, string(out))
	}
	return nil
}

// replaceLineInFile rewrites path in place, replacing the first regex match
// with replacement. It preserves the original file's mode bits via stat.
// Returns an error when the regex finds no match in the file (silent failure
// would leave the on-disk yaml inconsistent with the resized image).
func replaceLineInFile(path string, re *regexp.Regexp, replacement string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !re.Match(data) {
		return fmt.Errorf("no match for %s in %s", re, path)
	}
	out := re.ReplaceAll(data, []byte(replacement))
	return os.WriteFile(path, out, fi.Mode().Perm())
}
