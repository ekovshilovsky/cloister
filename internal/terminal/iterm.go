package terminal

import "fmt"

// setITermColor emits iTerm2's proprietary OSC Ph escape sequence to set the
// session background color. The argument is the hex digits of the color
// (e.g. "0a1628" — no leading "#"). This sequence is honored only by iTerm2
// and so callers should gate on a positive iTerm2 detection; other terminals
// would either ignore it silently or render its bytes literally into the
// scrollback.
func setITermColor(color string) {
	fmt.Printf("\033]Ph%s\033\\", color)
}
