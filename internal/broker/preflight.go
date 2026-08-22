// Proprietary and confidential. All rights reserved.

package broker

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	brokerignore "cloister.io/internal/broker/ignore"
)

// PreflightReport summarizes bounded, transient project inspection. The scan
// retains no descriptor or per-file bookkeeping after it returns.
type PreflightReport struct {
	Entries  uint64
	Warnings []string
}

type metadataInspector interface {
	Xattrs(string) ([]string, error)
}

type commandMetadataInspector struct{}

func (commandMetadataInspector) Xattrs(path string) ([]string, error) {
	return listXattrs(path)
}

// PreflightProject rejects filesystem features outside the synchronized-copy
// contract and warns about material xattrs that will remain host-only.
func PreflightProject(root string, policy brokerignore.Policy) (PreflightReport, error) {
	return PreflightProjectWithLimit(root, policy, 0)
}

func preflightProject(root string, policy brokerignore.Policy, inspector metadataInspector) (PreflightReport, error) {
	return preflightProjectWithLimit(root, policy, 0, inspector)
}

// PreflightProjectWithLimit stops as soon as the synchronized entry count
// exceeds the per-session Mutagen guardrail. A zero limit disables the check.
func PreflightProjectWithLimit(root string, policy brokerignore.Policy, maxEntries uint64) (PreflightReport, error) {
	return preflightProjectWithLimit(root, policy, maxEntries, commandMetadataInspector{})
}

func preflightProjectWithLimit(root string, policy brokerignore.Policy, maxEntries uint64, inspector metadataInspector) (PreflightReport, error) {
	original, err := filepath.Abs(root)
	if err != nil {
		return PreflightReport{}, fmt.Errorf("making project root absolute: %w", err)
	}
	originalInfo, err := os.Lstat(original)
	if err != nil {
		return PreflightReport{}, err
	}
	if originalInfo.Mode()&os.ModeSymlink != 0 {
		return PreflightReport{}, fmt.Errorf("project root %q is a symlink; broker roots must be real directories", root)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return PreflightReport{}, fmt.Errorf("resolving project root: %w", err)
	}
	rootInfo, err := os.Lstat(canonical)
	if err != nil {
		return PreflightReport{}, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return PreflightReport{}, fmt.Errorf("project root %q must be a real directory", canonical)
	}
	rootDevice, err := device(rootInfo)
	if err != nil {
		return PreflightReport{}, err
	}

	report := PreflightReport{}
	err = filepath.WalkDir(canonical, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == canonical {
			return nil
		}
		relative, err := filepath.Rel(canonical, path)
		if err != nil {
			return err
		}
		if policy.Ignored(relative, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		report.Entries++
		if maxEntries > 0 && report.Entries > maxEntries {
			return fmt.Errorf("project exceeds maxEntryCount %d", maxEntries)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		entryDevice, err := device(info)
		if err != nil {
			return err
		}
		if entryDevice != rootDevice {
			return fmt.Errorf("nested filesystem at %q is unsupported by synchronized copies", relative)
		}

		mode := info.Mode()
		if mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) || escapesRoot(relative, target) {
				return fmt.Errorf("symlink %q targets %q outside the project; broker mode requires portable relative symlinks", relative, target)
			}
			return nil
		}
		if !mode.IsRegular() && !mode.IsDir() {
			return fmt.Errorf("special file %q (%s) is unsupported by synchronized copies", relative, mode.Type())
		}
		if mode.IsRegular() {
			links, err := linkCount(info)
			if err != nil {
				return err
			}
			if links > 1 {
				return fmt.Errorf("hardlinked file %q has %d links; broker mode does not preserve hardlink identity", relative, links)
			}
			xattrs, err := inspector.Xattrs(path)
			if err != nil {
				return err
			}
			if len(xattrs) > 0 && len(report.Warnings) < 10 {
				report.Warnings = append(report.Warnings, fmt.Sprintf("%s has host-only extended attributes: %s", relative, strings.Join(xattrs, ", ")))
			}
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	return report, nil
}

func escapesRoot(relative, target string) bool {
	joined := filepath.Clean(filepath.Join(filepath.Dir(relative), target))
	return joined == ".." || strings.HasPrefix(joined, ".."+string(filepath.Separator))
}

func device(info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("filesystem metadata is unavailable for %q", info.Name())
	}
	return uint64(stat.Dev), nil
}

func linkCount(info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("hardlink metadata is unavailable for %q", info.Name())
	}
	return uint64(stat.Nlink), nil
}
