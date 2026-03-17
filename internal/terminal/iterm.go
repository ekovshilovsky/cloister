package terminal

import "fmt"

// setITermIdentity applies iTerm2-specific visual identity for the given
// profile. When a hex color is provided it is sent via the iTerm2 proprietary
// OSC sequence that changes the background color of the current session.
// The tab and window titles are also updated so the active profile is visible
// in the macOS Dock and the iTerm2 tab bar.
func setITermIdentity(profile string, color string) {
	if color != "" {
		// Set background color via iTerm2 proprietary OSC sequence (OSC Ph).
		fmt.Printf("\033]Ph%s\033\\", color)
	}
	// Set tab title to surface the profile identity alongside the tool name.
	fmt.Printf("\033]1;✱ Claude Code [%s]\007", profile)
	// Set window title so the profile is visible at the OS window-manager level.
	fmt.Printf("\033]2;cloister: %s\007", profile)
}
