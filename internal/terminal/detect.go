package terminal

import "os"

// Capabilities describes which proprietary terminal extensions the detected
// emulator is known to honor. False values mean either the extension is not
// supported or cloister has not been taught to detect support; either way the
// corresponding helper is skipped to avoid leaking unrecognized escape bytes
// into the scrollback.
type Capabilities struct {
	// UserVars indicates support for OSC 1337 SetUserVar=KEY=BASE64(VAL),
	// the iTerm2 proprietary user-variable mechanism. Variables set this
	// way persist for the lifetime of the terminal session, survive
	// subsequent OSC 0/1/2 title sets from other applications, and can
	// be referenced from the user's tab- or window-title format string
	// as "\(user.<KEY>)".
	UserVars bool
}

// Terminal records the identity and known capabilities of the active
// terminal emulator as inferred from environment variables. Name is empty
// when no recognized emulator was detected, signaling that callers should
// fall back to a generic strategy (printed banner) rather than emitting any
// proprietary sequences.
type Terminal struct {
	Name         string
	Capabilities Capabilities
}

// Detect identifies the active terminal emulator from the environment. The
// signals are checked from most-specific to least-specific, with explicit
// emulator advertisements (TERM_PROGRAM, KITTY_WINDOW_ID, etc.) preferred
// over TERM heuristics because TERM is frequently inherited from a parent
// shell or overridden by user customization and is therefore unreliable as
// a primary identifier.
func Detect() Terminal {
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app":
		return Terminal{Name: "iterm2", Capabilities: Capabilities{UserVars: true}}
	case "ghostty":
		return Terminal{Name: "ghostty", Capabilities: Capabilities{UserVars: true}}
	case "WezTerm":
		return Terminal{Name: "wezterm", Capabilities: Capabilities{UserVars: true}}
	case "WarpTerminal":
		return Terminal{Name: "warp"}
	case "Apple_Terminal":
		return Terminal{Name: "terminal-app"}
	case "vscode":
		return Terminal{Name: "vscode"}
	}

	// Some emulators set their own per-instance environment variables but
	// do not populate TERM_PROGRAM (or set it to a value that conflicts
	// with another emulator wrapping them, e.g. tmux inside iTerm). Fall
	// back to those next.
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return Terminal{Name: "kitty"}
	}
	if os.Getenv("WEZTERM_PANE") != "" {
		return Terminal{Name: "wezterm", Capabilities: Capabilities{UserVars: true}}
	}
	if os.Getenv("ALACRITTY_LOG") != "" || os.Getenv("ALACRITTY_WINDOW_ID") != "" {
		return Terminal{Name: "alacritty"}
	}

	// TERM-based heuristics last; honor them only when nothing more
	// specific has matched, because TERM is shell-inherited and may not
	// reflect the actual emulator.
	if os.Getenv("TERM") == "xterm-kitty" {
		return Terminal{Name: "kitty"}
	}

	return Terminal{}
}
