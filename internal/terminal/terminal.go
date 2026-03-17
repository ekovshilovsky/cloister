package terminal

import "os"

// SetIdentity applies visual identity for the current terminal emulator.
// On iTerm2 it sets the background accent color and window/tab titles; on all
// other terminals it prints a plain-text banner so the active profile is still
// clearly visible.
func SetIdentity(profile string, color string) {
	if isITerm() {
		setITermIdentity(profile, color)
	} else {
		setFallbackIdentity(profile)
	}
}

// isITerm reports whether the process is running inside iTerm2.
func isITerm() bool {
	return os.Getenv("TERM_PROGRAM") == "iTerm.app"
}
