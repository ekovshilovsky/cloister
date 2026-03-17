package terminal

import "fmt"

// setFallbackIdentity prints a plain-text banner identifying the active
// profile. This is used on terminals that do not support iTerm2 OSC sequences,
// ensuring the active profile context is always visible regardless of the
// terminal emulator in use.
func setFallbackIdentity(profile string) {
	fmt.Printf("\n═══ cloister: %s ═══\n\n", profile)
}
