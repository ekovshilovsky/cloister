// Proprietary and confidential. All rights reserved.

//go:build !darwin

package lifecycle

import "fmt"

// setTestXattr is unavailable off macOS, where the host-only attributes this
// warning is about do not arise.
func setTestXattr(string) error {
	return fmt.Errorf("extended attributes are only exercised on darwin")
}
