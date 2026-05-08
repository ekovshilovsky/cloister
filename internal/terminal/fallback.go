package terminal

import "fmt"

// printFallbackBanner emits a plain-text banner identifying the active
// profile. It is invoked only when no recognized terminal emulator was
// detected, on the assumption that such terminals may not honor OSC title
// sequences either; the inline banner ensures the profile context is at
// least visible in the scrollback. Recognized terminals already received
// standard OSC titles via setStandardTitles and the banner would just add
// scrollback noise on top of them.
func printFallbackBanner(profile string) {
	fmt.Printf("\n═══ cloister: %s ═══\n\n", profile)
}
