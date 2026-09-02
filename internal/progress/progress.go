// Proprietary and confidential. All rights reserved.

// Package progress reports what a long command is doing without printing the
// command's output.
//
// Provisioning spends minutes inside package managers whose output says a
// great deal to a package manager and very little to the person waiting. This
// reports the step in flight instead, so the console answers "what is it
// doing, and is it stuck" -- the two questions a scrolling wall of apt output
// answers only by accident.
package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// spinnerFrames are Braille cells because they occupy one column in every
// terminal that renders them, so the line does not shift width as it turns.
var spinnerFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

// Display renders step progress to a writer.
//
// An interactive display rewrites a single line in place. A plain one prints a
// line per step and never emits cursor control, because carriage returns and
// escape sequences reaching a pipe or a CI log render as garbage.
type Display struct {
	out         io.Writer
	interactive bool
	now         func() time.Time

	mu      sync.Mutex
	current *Step
	frame   int
	width   int
}

// Step is one unit of work with a name the reader recognizes.
type Step struct {
	display *Display
	name    string
	detail  string
	started time.Time
}

// New returns a Display writing to out. Pass interactive false whenever the
// destination is not a terminal.
func New(out io.Writer, interactive bool) *Display {
	return &Display{out: out, interactive: interactive, now: time.Now}
}

// Step begins a unit of work, finishing any step still in flight as done.
func (d *Display) Step(name string) *Step {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.current != nil {
		d.settleLocked(d.current, true)
	}
	step := &Step{display: d, name: name, started: d.now()}
	d.current = step
	if !d.interactive {
		fmt.Fprintf(d.out, "%s...\n", name)
		return step
	}
	d.renderLocked()
	return step
}

// Detail reports the sub-step in flight. It is shown only on an interactive
// display, where it costs nothing: the line is rewritten either way. A plain
// display drops it, since the run log already carries every sub-step and a
// line per one would rebuild the wall this package exists to remove.
func (s *Step) Detail(detail string) {
	if s == nil {
		return
	}
	s.display.mu.Lock()
	defer s.display.mu.Unlock()
	s.detail = detail
	if s.display.interactive && s.display.current == s {
		s.display.renderLocked()
	}
}

// Done marks the step finished.
func (s *Step) Done() { s.settle(true) }

// Fail marks the step failed.
func (s *Step) Fail() { s.settle(false) }

func (s *Step) settle(ok bool) {
	if s == nil {
		return
	}
	s.display.mu.Lock()
	defer s.display.mu.Unlock()
	s.display.settleLocked(s, ok)
}

func (d *Display) settleLocked(step *Step, ok bool) {
	if d.current != step {
		return
	}
	d.current = nil

	mark := "✓"
	if !ok {
		mark = "✗"
	}
	// The settled line reports the step, not whichever sub-step happened to be
	// running when it finished: the detail described work in flight, and once
	// the step is over it would name a moment rather than a result.
	line := fmt.Sprintf("  %s %s  %s", mark, step.name, formatElapsed(d.now().Sub(step.started)))
	if d.interactive {
		d.clearLocked()
	}
	fmt.Fprintln(d.out, line)
}

// Tick advances the spinner and redraws. A caller driving a real terminal
// calls it on a timer; nothing happens when no step is in flight.
func (d *Display) Tick() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.interactive || d.current == nil {
		return
	}
	d.frame++
	d.renderLocked()
}

func (d *Display) renderLocked() {
	step := d.current
	if step == nil {
		return
	}
	label := step.name
	if step.detail != "" {
		label += " · " + step.detail
	}
	line := fmt.Sprintf("%c %s  %s",
		spinnerFrames[d.frame%len(spinnerFrames)], label, formatElapsed(d.now().Sub(step.started)))

	// Pad to the widest line drawn so far rather than emitting an erase
	// sequence, so a shorter frame cannot leave the tail of a longer one
	// behind and the output stays readable when captured.
	if len(line) > d.width {
		d.width = len(line)
	}
	fmt.Fprintf(d.out, "\r%-*s", d.width, line)
}

func (d *Display) clearLocked() {
	if d.width == 0 {
		return
	}
	fmt.Fprintf(d.out, "\r%s\r", strings.Repeat(" ", d.width))
	d.width = 0
}

// formatElapsed renders a duration at the resolution a person waiting cares
// about: whole seconds, and minutes once there are any.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm%ds", seconds/60, seconds%60)
}
