// Proprietary and confidential. All rights reserved.

// Package report is the seam between a provisioning engine and whatever is
// showing its progress.
//
// The engines run the work; the command layer owns the console and the run log.
// Neither can import the other, and the interface cannot be declared twice at
// the two points of use: a method's return type is invariant in Go, so one
// session could not satisfy both a linux.Reporter and a macos.Reporter. It
// therefore lives here, where every provisioning package can reach it.
package report

import "io"

// Reporter hands an operation the steps it reports through, without the
// operation knowing whether they are drawn on a terminal, or where the output
// they produce is kept.
type Reporter interface {
	// Step begins a unit of work. The name is what the reader sees, so it
	// names the work ("Base tools"), not the mechanism.
	Step(name string) Step
}

// Step is one unit of work: somewhere to send its guest output, and the three
// ways it can end.
type Step interface {
	// Writer is the destination for this step's guest output.
	Writer() io.Writer

	// Done marks the step successful.
	Done()

	// Warn marks the step as having reported a problem it carried on past.
	// Provisioning has several of these -- a profile whose GPG setup fails
	// still has a usable VM -- and calling them either success or failure
	// misreports what happened.
	Warn(message string)

	// Fail marks the step as the one that stopped the run.
	Fail()
}

// Discarded is the Reporter for a caller that supplied none. Steps go
// unreported and guest output goes to Out, which keeps the engines free of a
// nil check at every step. A caller that wants the work reported supplies a
// real Reporter; this one is the documented degenerate case, not a display.
type Discarded struct {
	// Out receives the guest output of every step. A nil Out discards it.
	Out io.Writer
}

// Step returns a step that records nothing.
func (d Discarded) Step(string) Step { return discardedStep{out: d.Out} }

type discardedStep struct{ out io.Writer }

func (s discardedStep) Writer() io.Writer {
	if s.out == nil {
		return io.Discard
	}
	return s.out
}

func (s discardedStep) Done()       {}
func (s discardedStep) Warn(string) {}
func (s discardedStep) Fail()       {}
