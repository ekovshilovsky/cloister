package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cloister.io/internal/config"
	"cloister.io/internal/progress"
	"cloister.io/internal/provision/report"
	"cloister.io/internal/runlog"
	"github.com/spf13/cobra"
)

// The provisioning engines take their steps from the session through this
// interface, which is what keeps them free of any knowledge of the console.
var _ report.Reporter = (*provisionSession)(nil)

// addVerboseFlag registers the flag that puts the guest output back on the
// console. The run log is written either way: verbose is for watching a
// provision live, not for choosing between the two records.
func addVerboseFlag(cmd *cobra.Command, verbose *bool) {
	cmd.Flags().BoolVar(verbose, "verbose", false,
		"Stream guest output to the console as well as the run log")
}

// failureTailLines is how much of a failed step is replayed to the console.
// Enough to carry the error and the lines that led to it, short enough that it
// does not become the wall the run log exists to absorb.
const failureTailLines = 40

// spinnerInterval is how often the live line is redrawn. Fast enough to read
// as motion, slow enough to cost nothing.
const spinnerInterval = 100 * time.Millisecond

// provisionSession routes one command's guest output to a run log and reports
// its progress on the console.
//
// The console summarizes progress while the run log retains complete package
// manager output. A failure replays only the bounded diagnostic tail.
type provisionSession struct {
	run       *runlog.Run
	log       io.Writer
	echo      io.Writer
	display   *progress.Display
	done      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
}

// startProvisionSession opens a run log for the profile and begins reporting
// progress. A session that cannot open its log still reports progress: losing
// the record is worth a warning, not a refused command.
//
// A verbose session also streams the guest output to the console, for watching
// a provision as it happens.
func startProvisionSession(profile, command string, verbose bool) *provisionSession {
	var log io.Writer
	var run *runlog.Run

	if dir, err := logDir(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: no run log for this %s: %v\n", command, err)
	} else if opened, err := runlog.Open(dir, profile, command); err != nil {
		fmt.Fprintf(os.Stderr, "warning: no run log for this %s: %v\n", command, err)
	} else {
		run, log = opened, opened.Writer()
	}

	session := newProvisionSession(os.Stdout, log, isInteractive(), verbose)
	session.run = run
	return session
}

// newProvisionSession assembles a session over destinations the caller has
// already resolved, so the routing can be exercised without a terminal or a log
// file on disk. A nil log means this session has nowhere to record.
func newProvisionSession(console io.Writer, log io.Writer, interactive, verbose bool) *provisionSession {
	// A spinner rewrites the line it would be sharing with the stream, so the
	// two cannot both drive the console. Verbose exists to read the output, and
	// plain rendering leaves it readable.
	if verbose {
		interactive = false
	}

	session := &provisionSession{log: log, display: progress.New(console, interactive)}
	if verbose {
		session.echo = console
	}

	if interactive {
		session.done = make(chan struct{})
		session.stopped = make(chan struct{})
		go func() {
			defer close(session.stopped)
			ticker := time.NewTicker(spinnerInterval)
			defer ticker.Stop()
			for {
				select {
				case <-session.done:
					return
				case <-ticker.C:
					session.display.Tick()
				}
			}
		}()
	}
	return session
}

// Step begins a unit of work. Write the guest output for that unit to the
// returned step's Writer.
//
// The interface rather than the concrete step is what a provisioning engine
// consumes, and a method's return type is invariant, so this is the type the
// seam requires.
func (s *provisionSession) Step(name string) report.Step {
	logWriter := s.destination()
	step := s.display.Step(name)
	// The sink reads the banners the scripts already print, so the live line
	// can name the sub-step running rather than freezing on the outer label
	// for the minutes a script takes.
	sink := runlog.NewSink(logWriter, step.Detail, failureTailLines)
	return &provisionStep{session: s, step: step, sink: sink}
}

// destination is where a step's guest output goes: the run log, the console as
// well when the session is verbose, and nowhere when it has neither.
func (s *provisionSession) destination() io.Writer {
	switch {
	case s.log != nil && s.echo != nil:
		return io.MultiWriter(s.log, s.echo)
	case s.echo != nil:
		return s.echo
	default:
		return s.log
	}
}

// LogPath is where this session's output was recorded, or "" if it has no log.
func (s *provisionSession) LogPath() string {
	if s.run == nil {
		return ""
	}
	return s.run.Path()
}

// Close stops progress reporting and releases the run log.
func (s *provisionSession) Close() {
	s.closeOnce.Do(func() {
		// The stop channels are immutable after construction. Closing once and
		// waiting for acknowledgement makes repeated or concurrent Close calls
		// safe and ensures the spinner has stopped touching its display.
		if s.done != nil {
			close(s.done)
			<-s.stopped
		}
		if s.run != nil {
			s.run.Close()
		}
	})
}

// provisionStep is one unit of work and the destination for its guest output.
type provisionStep struct {
	session *provisionSession
	step    *progress.Step
	sink    *runlog.Sink
}

// Writer is where this step's guest output goes.
func (s *provisionStep) Writer() io.Writer { return s.sink }

// Done marks the step successful.
func (s *provisionStep) Done() { s.step.Done() }

// Warn marks the step as having reported a problem it carried on past. No tail
// is replayed: the step did not stop the run, so the message it carries is the
// whole story and the run log holds the rest.
func (s *provisionStep) Warn(message string) { s.step.Warn(message) }

// Fail marks the step failed and replays the end of its output.
//
// A failed step that says only "see the log" trades one kind of unhelpfulness
// for another: the reader has to run a second command before learning what
// broke. The tail puts the error back on the console while the bulk stays on
// disk.
func (s *provisionStep) Fail() {
	s.step.Fail()
	tail := s.sink.Tail()
	if len(tail) > 0 {
		fmt.Fprintf(os.Stderr, "\n  last %d lines:\n", len(tail))
		for _, line := range tail {
			fmt.Fprintf(os.Stderr, "  │ %s\n", line)
		}
	}
	if path := s.session.LogPath(); path != "" {
		fmt.Fprintf(os.Stderr, "\n  full log: %s\n", path)
	}
}

// logDir is where run logs are kept, alongside the rest of cloister's state.
func logDir() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving config dir: %w", err)
	}
	return filepath.Join(dir, "logs"), nil
}
