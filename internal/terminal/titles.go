package terminal

import "fmt"

// setStandardTitles emits OSC 1 (icon/tab title) and OSC 2 (window title) so
// the active profile is visible in the tab strip and OS window header on
// every terminal that implements the long-standing xterm conventions. Both
// titles are subject to last-write-wins overwrite by any application that
// subsequently sets a title via OSC 0/1/2 (Claude Code, ble.sh, ssh, etc.);
// the SetUserVar path in uservars.go is what survives such overwrites for
// terminals that support OSC 1337.
func setStandardTitles(profile string) {
	fmt.Printf("\033]1;✱ Claude Code [%s]\007", profile)
	fmt.Printf("\033]2;cloister: %s\007", profile)
}
