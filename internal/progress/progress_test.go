// Proprietary and confidential. All rights reserved.

package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// fixedClock advances only when a test says so, so rendered durations are
// deterministic.
type fixedClock struct{ at time.Time }

func (c *fixedClock) now() time.Time          { return c.at }
func (c *fixedClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newTestDisplay(interactive bool) (*Display, *bytes.Buffer, *fixedClock) {
	var out bytes.Buffer
	clock := &fixedClock{at: time.Unix(0, 0)}
	display := New(&out, interactive)
	display.now = clock.now
	return display, &out, clock
}

func TestPlainDisplayIsDeterministicAndFreeOfControlCodes(t *testing.T) {
	display, out, clock := newTestDisplay(false)

	step := display.Step("Base tools")
	clock.advance(72 * time.Second)
	step.Done()

	got := out.String()
	want := "Base tools...\n  ✓ Base tools  1m12s\n"
	if got != want {
		t.Errorf("plain output = %q, want %q", got, want)
	}
	// A pipe or CI log must not receive cursor control: carriage returns and
	// escape sequences render as garbage once the output is not a terminal.
	if strings.ContainsAny(got, "\r\x1b") {
		t.Errorf("plain output carries control codes: %q", got)
	}
}

func TestPlainDisplayMarksFailure(t *testing.T) {
	display, out, clock := newTestDisplay(false)

	step := display.Step("agentgrid stack")
	clock.advance(3 * time.Second)
	step.Fail()

	if got, want := out.String(), "agentgrid stack...\n  ✗ agentgrid stack  3s\n"; got != want {
		t.Errorf("plain output = %q, want %q", got, want)
	}
}

func TestPlainDisplayOmitsSubStepDetail(t *testing.T) {
	display, out, _ := newTestDisplay(false)

	step := display.Step("Base tools")
	step.Detail("Installing Node.js via NVM")
	step.Detail("Installing Claude Code")
	step.Done()

	if strings.Contains(out.String(), "Node.js") {
		t.Errorf("plain output should not carry sub-step detail, got %q", out.String())
	}
}

func TestInteractiveDisplayShowsDetailOnOneRewrittenLine(t *testing.T) {
	display, out, clock := newTestDisplay(true)

	step := display.Step("Base tools")
	step.Detail("Installing Node.js via NVM")
	clock.advance(22 * time.Second)
	display.Tick()

	live := out.String()
	if !strings.Contains(live, "Base tools") || !strings.Contains(live, "Installing Node.js via NVM") {
		t.Errorf("interactive frame missing step or detail: %q", live)
	}
	if !strings.Contains(live, "22s") {
		t.Errorf("interactive frame missing elapsed time: %q", live)
	}
	// The line is rewritten in place rather than appended to, so progress does
	// not itself become the scrolling wall it replaces.
	if !strings.Contains(live, "\r") {
		t.Errorf("interactive frame does not rewrite its line: %q", live)
	}
	if strings.Count(live, "\n") != 0 {
		t.Errorf("interactive frame ended a line before the step finished: %q", live)
	}

	out.Reset()
	step.Done()
	final := out.String()
	if !strings.Contains(final, "✓ Base tools") {
		t.Errorf("finished step missing its mark: %q", final)
	}
	if !strings.HasSuffix(final, "\n") {
		t.Errorf("finished step did not end its line: %q", final)
	}
	// The detail belonged to the step in flight; the settled line reports the
	// step, not whichever sub-step happened to be last.
	if strings.Contains(final, "Node.js") {
		t.Errorf("finished step still shows transient detail: %q", final)
	}
}

func TestInteractiveSpinnerAdvancesBetweenTicks(t *testing.T) {
	display, out, _ := newTestDisplay(true)
	display.Step("Base tools")

	out.Reset()
	display.Tick()
	first := out.String()
	out.Reset()
	display.Tick()
	second := out.String()

	if first == second {
		t.Errorf("spinner frame did not advance between ticks: %q", first)
	}
}

func TestTickAfterTheLastStepWritesNothing(t *testing.T) {
	display, out, _ := newTestDisplay(true)
	display.Step("Base tools").Done()

	out.Reset()
	display.Tick()
	if out.String() != "" {
		t.Errorf("Tick() with no step in flight wrote %q", out.String())
	}
}

func TestFormatElapsed(t *testing.T) {
	for _, testCase := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{999 * time.Millisecond, "0s"},
		{3 * time.Second, "3s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m0s"},
		{72 * time.Second, "1m12s"},
		{3661 * time.Second, "61m1s"},
	} {
		if got := formatElapsed(testCase.in); got != testCase.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}

func TestPlainDisplayMarksWarnings(t *testing.T) {
	display, out, clock := newTestDisplay(false)

	// Some provisioning steps report a problem and carry on: a profile whose
	// GPG setup fails still has a usable VM, so the run must be able to say so
	// without claiming success or stopping.
	step := display.Step("GPG isolation")
	clock.advance(4 * time.Second)
	step.Warn("GPG setup: no secret key")

	want := "GPG isolation...\n  ⚠ GPG isolation  4s\n    GPG setup: no secret key\n"
	if got := out.String(); got != want {
		t.Errorf("plain output = %q, want %q", got, want)
	}
}

func TestInteractiveDisplayMarksWarnings(t *testing.T) {
	display, out, _ := newTestDisplay(true)

	step := display.Step("GPG isolation")
	out.Reset()
	step.Warn("GPG setup: no secret key")

	got := out.String()
	if !strings.Contains(got, "⚠ GPG isolation") {
		t.Errorf("warned step missing its mark: %q", got)
	}
	if !strings.Contains(got, "GPG setup: no secret key") {
		t.Errorf("warned step missing its message: %q", got)
	}
}

func TestWarnWithoutAMessageJustMarksTheStep(t *testing.T) {
	display, out, _ := newTestDisplay(false)
	display.Step("GPG isolation").Warn("")

	if got, want := out.String(), "GPG isolation...\n  ⚠ GPG isolation  0s\n"; got != want {
		t.Errorf("plain output = %q, want %q", got, want)
	}
}

func TestWarnReportsOnlyTheFirstLineOfItsMessage(t *testing.T) {
	display, out, _ := newTestDisplay(false)

	// Wrapped provisioning errors carry the failing command's output after a
	// newline. Printing it here breaks out of the step's indentation and
	// rebuilds, one warning at a time, the wall of guest output this display
	// exists to remove. The run log already holds it.
	step := display.Step("GitHub CLI authentication")
	step.Warn("gh auth: ssh script failed: exit status 1\nOutput: level=fatal msg=\"exit status 127\"\n")

	got := out.String()
	want := "GitHub CLI authentication...\n  ⚠ GitHub CLI authentication  0s\n    gh auth: ssh script failed: exit status 1\n"
	if got != want {
		t.Errorf("plain output = %q, want %q", got, want)
	}
	if strings.Count(got, "\n") != 3 {
		t.Errorf("warning spans %d lines, want 3", strings.Count(got, "\n"))
	}
}
