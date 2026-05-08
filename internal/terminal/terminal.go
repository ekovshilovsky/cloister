// Package terminal stamps cloister's per-profile visual identity onto the
// active terminal emulator. It dispatches between standard OSC title
// sequences (which every modern terminal honors but which any subsequent
// application can overwrite) and a small set of proprietary extensions
// (notably iTerm2's OSC 1337 SetUserVar) that survive title overwrites
// when the user references them from a tab-title format string.
package terminal

// SetIdentity emits visual identity for the named cloister profile, tailored
// to the detected terminal emulator. Every detected terminal receives standard
// OSC 1 (icon/tab title) and OSC 2 (window title) sequences. iTerm2-extension-
// family terminals (iTerm2, Ghostty, WezTerm) additionally receive an OSC 1337
// SetUserVar=cloisterProfile=BASE64(profile) sequence so the profile name can
// be referenced from the user's tab-title format string and is not erased
// when an application inside the VM (Claude Code, ble.sh, etc.) writes a new
// title. iTerm2 also receives the proprietary OSC Ph background-color
// sequence when color is non-empty. Unknown terminals receive a printed
// banner in the scrollback so the profile context is still visible inline.
func SetIdentity(profile, color string) {
	t := Detect()

	// Standard OSC titles always go out so terminals that don't expose user
	// variables still surface the profile in their tab strip and window
	// header (subject to overwrite by subsequent application title sets).
	setStandardTitles(profile)

	// iTerm2's proprietary background-color escape is honored only by
	// iTerm2 itself; other terminals would either ignore the sequence or
	// render its bytes literally.
	if t.Name == "iterm2" && color != "" {
		setITermColor(color)
	}

	// SetUserVar is the only mechanism that survives subsequent OSC 0/1/2
	// title overwrites; gate emission on detected support so terminals that
	// would render the OSC 1337 bytes literally don't pollute the screen.
	if t.Capabilities.UserVars {
		setUserVar("cloisterProfile", profile)
	}

	// Fall back to a printed banner only when no known emulator was
	// detected; recognized terminals already received standard OSC titles
	// above and the banner would just add scrollback noise.
	if t.Name == "" {
		printFallbackBanner(profile)
	}
}
