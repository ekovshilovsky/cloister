// Proprietary and confidential. All rights reserved.

package runlog

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestSinkPassesEverythingThrough(t *testing.T) {
	var logged bytes.Buffer
	sink := NewSink(&logged, nil, 10)

	input := "Get:1 http://ports.ubuntu.com noble InRelease\nReading package lists...\n"
	if _, err := sink.Write([]byte(input)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if logged.String() != input {
		t.Errorf("logged = %q, want %q", logged.String(), input)
	}
}

func TestSinkReportsSectionMarkers(t *testing.T) {
	var sections []string
	sink := NewSink(&bytes.Buffer{}, func(s string) { sections = append(sections, s) }, 10)

	sink.Write([]byte("=== Installing base tools ===\n"))
	sink.Write([]byte("Get:1 http://ports.ubuntu.com noble InRelease\n"))
	sink.Write([]byte("  === Installing Claude Code ===\n"))
	sink.Write([]byte("not === a marker\n"))
	sink.Write([]byte("=== unbalanced ==\n"))

	want := []string{"Installing base tools", "Installing Claude Code"}
	if !reflect.DeepEqual(sections, want) {
		t.Errorf("sections = %v, want %v", sections, want)
	}
}

func TestSinkHandlesLinesSplitAcrossWrites(t *testing.T) {
	var sections []string
	var logged bytes.Buffer
	sink := NewSink(&logged, func(s string) { sections = append(sections, s) }, 10)

	// A pipe delivers whatever the kernel had ready, so a marker can arrive in
	// pieces and must still be recognized once complete.
	sink.Write([]byte("=== Installing "))
	sink.Write([]byte("op-forward (1Password"))
	sink.Write([]byte(" CLI forwarding) ===\nnext line\n"))

	want := []string{"Installing op-forward (1Password CLI forwarding)"}
	if !reflect.DeepEqual(sections, want) {
		t.Errorf("sections = %v, want %v", sections, want)
	}
	if !strings.HasSuffix(logged.String(), "next line\n") {
		t.Errorf("logged = %q, want the split write reassembled verbatim", logged.String())
	}
}

func TestSinkTailKeepsTheMostRecentLines(t *testing.T) {
	sink := NewSink(&bytes.Buffer{}, nil, 3)
	for _, line := range []string{"one", "two", "three", "four", "five"} {
		sink.Write([]byte(line + "\n"))
	}

	want := []string{"three", "four", "five"}
	if got := sink.Tail(); !reflect.DeepEqual(got, want) {
		t.Errorf("Tail() = %v, want %v", got, want)
	}
}

func TestSinkTailIncludesAnUnterminatedFinalLine(t *testing.T) {
	sink := NewSink(&bytes.Buffer{}, nil, 5)
	// A command killed mid-line still reports what it managed to say, and that
	// fragment is often the error worth reading.
	sink.Write([]byte("finished line\nE: Unable to locate package"))

	want := []string{"finished line", "E: Unable to locate package"}
	if got := sink.Tail(); !reflect.DeepEqual(got, want) {
		t.Errorf("Tail() = %v, want %v", got, want)
	}
}

func TestSinkTailBelowCapacityReturnsEverything(t *testing.T) {
	sink := NewSink(&bytes.Buffer{}, nil, 10)
	sink.Write([]byte("only\n"))

	if got := sink.Tail(); !reflect.DeepEqual(got, []string{"only"}) {
		t.Errorf("Tail() = %v, want [only]", got)
	}
}

func TestSinkStripsCarriageReturns(t *testing.T) {
	sink := NewSink(&bytes.Buffer{}, nil, 5)
	// Guest tooling draws progress with carriage returns; keeping them makes a
	// replayed tail overwrite itself in the reader's terminal.
	sink.Write([]byte("downloading 50%\rdownloading 100%\ndone\n"))

	got := sink.Tail()
	for _, line := range got {
		if strings.Contains(line, "\r") {
			t.Errorf("Tail() line %q carries a carriage return", line)
		}
	}
}

func TestSinkHandlesCRLFLineEndings(t *testing.T) {
	var sections []string
	sink := NewSink(&bytes.Buffer{}, func(s string) { sections = append(sections, s) }, 5)

	// Output that crossed a pseudo-terminal arrives CRLF-terminated. Treating
	// that trailing carriage return as a progress redraw would discard the
	// whole line, which is every line.
	sink.Write([]byte("=== Installing base tools ===\r\nReading package lists...\r\n"))

	if !reflect.DeepEqual(sections, []string{"Installing base tools"}) {
		t.Errorf("sections = %v, want [Installing base tools]", sections)
	}
	if got := sink.Tail(); !reflect.DeepEqual(got, []string{"=== Installing base tools ===", "Reading package lists..."}) {
		t.Errorf("Tail() = %q, want the lines intact", got)
	}
}
