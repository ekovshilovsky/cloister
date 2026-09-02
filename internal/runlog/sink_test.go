// Proprietary and confidential. All rights reserved.

package runlog

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestSinkPassesTextWithoutCarriageReturnsThroughByteIdentical(t *testing.T) {
	var logged bytes.Buffer
	sink := NewSink(&logged, nil, 10)

	parts := []string{
		"Get:1 http://ports.ubuntu.com noble InRelease\n",
		"Reading package ",
		"lists...\nunterminated output",
	}
	for _, part := range parts {
		if _, err := sink.Write([]byte(part)); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	input := strings.Join(parts, "")
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
	var logged bytes.Buffer
	sink := NewSink(&logged, nil, 5)
	// Guest tooling draws progress with carriage returns; keeping them makes a
	// replayed tail overwrite itself in the reader's terminal.
	sink.Write([]byte("downloading 50%\rdownloading 100%\ndone\n"))
	if got, want := logged.String(), "downloading 100%\ndone\n"; got != want {
		t.Errorf("logged = %q, want %q", got, want)
	}

	got := sink.Tail()
	for _, line := range got {
		if strings.Contains(line, "\r") {
			t.Errorf("Tail() line %q carries a carriage return", line)
		}
	}
}

func TestSinkHandlesCRLFLineEndings(t *testing.T) {
	var sections []string
	var logged bytes.Buffer
	sink := NewSink(&logged, func(s string) { sections = append(sections, s) }, 5)

	// Output that crossed a pseudo-terminal arrives CRLF-terminated. Treating
	// that trailing carriage return as a progress redraw would discard the
	// whole line, which is every line.
	input := "=== Installing base tools ===\r\nReading package lists...\r\n"
	sink.Write([]byte(input))

	if logged.String() != input {
		t.Errorf("logged = %q, want CRLF input byte-identical", logged.String())
	}

	if !reflect.DeepEqual(sections, []string{"Installing base tools"}) {
		t.Errorf("sections = %v, want [Installing base tools]", sections)
	}
	if got := sink.Tail(); !reflect.DeepEqual(got, []string{"=== Installing base tools ===", "Reading package lists..."}) {
		t.Errorf("Tail() = %q, want the lines intact", got)
	}
}

// The tail is what reaches the console, so a line long enough to be a wall on
// its own has to be cut whether or not it ever arrives complete.
func TestSinkTruncatesALineLongerThanTheLineCeiling(t *testing.T) {
	sink := NewSink(&bytes.Buffer{}, nil, 5)

	// One megabyte with no newline: a command killed mid-line, or a guest
	// dumping a file. Every byte still reaches the log; only the replay is cut.
	var logged bytes.Buffer
	sink = NewSink(&logged, nil, 5)
	overlong := strings.Repeat("x", 1<<20)
	if _, err := sink.Write([]byte(overlong)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if logged.Len() != len(overlong) {
		t.Errorf("log received %d bytes, want the full %d", logged.Len(), len(overlong))
	}
	tail := sink.Tail()
	if len(tail) != 1 {
		t.Fatalf("Tail() = %d lines, want 1", len(tail))
	}
	if len(tail[0]) > maxTailLineBytes {
		t.Errorf("Tail() line is %d bytes, want at most %d", len(tail[0]), maxTailLineBytes)
	}
	if !strings.Contains(tail[0], "truncated") {
		// Reporting the line itself would reproduce the wall under test.
		t.Errorf("Tail() line does not say it was cut; it ends %q", tail[0][max(0, len(tail[0])-64):])
	}
}

// A bound on lines is not a bound on bytes: many long lines defeat it just as
// one enormous line does.
func TestSinkBoundsTheTotalBytesItRetains(t *testing.T) {
	sink := NewSink(&bytes.Buffer{}, nil, 40)

	for i := 0; i < 40; i++ {
		sink.Write([]byte(strings.Repeat("y", 4096) + "\n"))
	}

	total := 0
	for _, line := range sink.Tail() {
		total += len(line)
	}
	if total > maxTailBytes {
		t.Errorf("Tail() holds %d bytes, want at most %d", total, maxTailBytes)
	}
	if total == 0 {
		t.Error("Tail() is empty; the bound must keep the most recent output, not discard it")
	}
}

// The ceiling must not cost the reader the newest output: what survives is the
// end of the stream, which is where a failure reports itself.
func TestSinkBoundedTailKeepsTheMostRecentLines(t *testing.T) {
	sink := NewSink(&bytes.Buffer{}, nil, 40)

	for i := 0; i < 40; i++ {
		sink.Write([]byte(strings.Repeat("z", 4096) + "\n"))
	}
	sink.Write([]byte("E: the error worth reading\n"))

	tail := sink.Tail()
	if len(tail) == 0 || tail[len(tail)-1] != "E: the error worth reading" {
		t.Errorf("Tail() does not end with the newest line: %v", tail[max(0, len(tail)-1):])
	}
}

// An overlong line arriving in pieces is the same line, so the ceiling applies
// to the assembled result rather than to each write.
func TestSinkTruncatesAnOverlongLineSplitAcrossWrites(t *testing.T) {
	sink := NewSink(&bytes.Buffer{}, nil, 5)

	for i := 0; i < 64; i++ {
		sink.Write([]byte(strings.Repeat("w", 4096)))
	}
	sink.Write([]byte("\n"))

	tail := sink.Tail()
	if len(tail) != 1 {
		t.Fatalf("Tail() = %d lines, want 1", len(tail))
	}
	if len(tail[0]) > maxTailLineBytes {
		t.Errorf("Tail() line is %d bytes, want at most %d", len(tail[0]), maxTailLineBytes)
	}
}

// Truncation cuts bytes, and a multi-byte character split down the middle
// renders as a replacement character rather than as what the guest printed.
func TestSinkTruncationDoesNotSplitAMultiByteCharacter(t *testing.T) {
	sink := NewSink(&bytes.Buffer{}, nil, 5)

	// "é" is two bytes, so some multiple of it lands the ceiling mid-character.
	sink.Write([]byte(strings.Repeat("é", maxTailLineBytes) + "\n"))

	tail := sink.Tail()
	if len(tail) != 1 {
		t.Fatalf("Tail() = %d lines, want 1", len(tail))
	}
	if strings.ContainsRune(tail[0], utf8.RuneError) {
		t.Errorf("Tail() line was cut mid-character: %q", tail[0])
	}
}

// A progress indicator redrawing in place emits megabytes that were never more
// than one line wide, so the accumulator must not grow with the redraws.
func TestSinkRedrawnProgressLineIsNotTruncated(t *testing.T) {
	var logged bytes.Buffer
	sink := NewSink(&logged, nil, 5)

	for i := 0; i < 4096; i++ {
		sink.Write([]byte(fmt.Sprintf("downloading %d/4096\r", i)))
	}
	sink.Write([]byte("downloading 4096/4096\ndone\n"))

	tail := sink.Tail()
	want := []string{"downloading 4096/4096", "done"}
	if !reflect.DeepEqual(tail, want) {
		t.Errorf("Tail() = %v, want %v", tail, want)
	}
	if got, want := logged.String(), "downloading 4096/4096\ndone\n"; got != want {
		t.Errorf("logged = %q, want %q", got, want)
	}
}

func TestSinkDetectsSectionImmediatelyAfterProgress(t *testing.T) {
	var logged bytes.Buffer
	var sections []string
	sink := NewSink(&logged, func(section string) { sections = append(sections, section) }, 5)

	sink.Write([]byte("downloading 1/3\r"))
	sink.Write([]byte("downloading 2/3\r"))
	sink.Write([]byte("downloading 3/3\n=== Installing packages ===\n"))

	if got, want := logged.String(), "downloading 3/3\n=== Installing packages ===\n"; got != want {
		t.Errorf("logged = %q, want %q", got, want)
	}
	if want := []string{"Installing packages"}; !reflect.DeepEqual(sections, want) {
		t.Errorf("sections = %v, want %v", sections, want)
	}
}

func TestSinkTailBoundsHoldWithProgressInput(t *testing.T) {
	writeProgress := func(sink *Sink) {
		for i := 0; i < 4096; i++ {
			sink.Write([]byte(fmt.Sprintf("download frame %d\r", i)))
		}
		sink.Write([]byte("download complete\n"))
	}

	t.Run("line count", func(t *testing.T) {
		sink := NewSink(io.Discard, nil, 3)
		writeProgress(sink)
		for _, line := range []string{"one", "two", "three", "four"} {
			sink.Write([]byte(line + "\n"))
		}
		if got, want := sink.Tail(), []string{"two", "three", "four"}; !reflect.DeepEqual(got, want) {
			t.Errorf("Tail() = %v, want %v", got, want)
		}
	})

	t.Run("total bytes", func(t *testing.T) {
		sink := NewSink(io.Discard, nil, 40)
		writeProgress(sink)
		for i := 0; i < 40; i++ {
			sink.Write([]byte(fmt.Sprintf("%02d %s\n", i, strings.Repeat("b", 1024))))
		}
		total := 0
		for _, line := range sink.Tail() {
			total += len(line)
		}
		if total > maxTailBytes {
			t.Errorf("Tail() holds %d bytes, want at most %d", total, maxTailBytes)
		}
	})

	t.Run("single line bytes", func(t *testing.T) {
		sink := NewSink(io.Discard, nil, 3)
		writeProgress(sink)
		sink.Write([]byte(strings.Repeat("x", maxTailLineBytes+1) + "\n"))
		line := sink.Tail()[1]
		if len(line) > maxTailLineBytes {
			t.Errorf("Tail() line holds %d bytes, want at most %d", len(line), maxTailLineBytes)
		}
	})
}

func TestSinkRealisticProgressLogMeasurement(t *testing.T) {
	const frameCount = 3000
	prefix := "Downloading Agent Grid package...\n"
	suffix := "Download complete; verifying package.\n"
	var input bytes.Buffer
	input.WriteString(prefix)
	lastFrame := ""
	for i := 0; i < frameCount; i++ {
		lastFrame = fmt.Sprintf(
			"%3d  567M %3d  %3dM    0     0   258k      0  0:37:23  0:07:%02d  0:29:48  287k",
			i*100/(frameCount-1), i*100/(frameCount-1), i*567/(frameCount-1), i%60,
		)
		input.WriteByte('\r')
		input.WriteString(lastFrame)
	}
	input.WriteByte('\n')
	input.WriteString(suffix)

	var logged bytes.Buffer
	sink := NewSink(&logged, nil, 10)
	raw := input.Bytes()
	for len(raw) > 0 {
		size := min(113, len(raw))
		if _, err := sink.Write(raw[:size]); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		raw = raw[size:]
	}

	want := prefix + lastFrame + "\n" + suffix
	if logged.String() != want {
		t.Fatalf("surrounding output or final progress frame changed:\n got %q\nwant %q", logged.String(), want)
	}
	t.Logf("realistic progress log: %d bytes before, %d bytes after", input.Len(), logged.Len())
}

func TestSinkConcurrentWrites(t *testing.T) {
	var logged bytes.Buffer
	sink := NewSink(&logged, nil, 40)
	var writers sync.WaitGroup

	for worker := 0; worker < 8; worker++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for frame := 0; frame < 100; frame++ {
				input := fmt.Sprintf("worker %d pending %d\rworker %d done %d\n", worker, frame, worker, frame)
				if _, err := sink.Write([]byte(input)); err != nil {
					t.Errorf("Write() error = %v", err)
				}
				_ = sink.Tail()
			}
		}()
	}
	writers.Wait()

	if got, want := bytes.Count(logged.Bytes(), []byte{'\n'}), 800; got != want {
		t.Errorf("log has %d complete lines, want %d", got, want)
	}
}
