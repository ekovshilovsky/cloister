// Proprietary and confidential. All rights reserved.

//go:build darwin

package lifecycle

import "os/exec"

// setTestXattr attaches an extended attribute preflight must report.
//
// Deliberately an unrecognized name rather than a macOS one: an attribute
// describing a file's relationship to this Mac (Spotlight, provenance,
// quarantine) cannot mean anything in the guest, and a preflight that
// distinguishes material attributes from ambient ones should stop reporting
// those. Pinning one here would make this test forbid that fix.
func setTestXattr(path string) error {
	return exec.Command("xattr", "-w", "com.example.novel", "test", path).Run()
}
