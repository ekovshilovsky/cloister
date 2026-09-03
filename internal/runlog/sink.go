// Proprietary and confidential. All rights reserved.

package runlog

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// sectionMarker is the banner the provisioning scripts already print around
// each unit of work, for example:
//
//	=== Installing op-forward (1Password CLI forwarding) ===
//
// Reading them is what lets a step in the console name the sub-step actually
// running, without the scripts having to learn anything new.
const sectionMarker = "==="

// A line count bounds how much of the output is retained only if the lines are
// the length lines usually are. One guest command dumping a file, or dying
// mid-line, produces a single line of any size at all, so the tail is bounded
// in bytes as well: the console is the thing being protected, and the console
// measures its wall in bytes.
const (
	// maxTailLineBytes is the ceiling on one retained line. Wide enough for a
	// package manager's longest real message, short enough that a line that
	// exceeds it was never meant to be read on a terminal.
	maxTailLineBytes = 2 << 10

	// maxTailBytes is the ceiling on the whole retained tail. At the forty
	// lines a failure replays, this is far more than ordinary output needs and
	// still less than a screen's worth of scrollback.
	maxTailBytes = 16 << 10
)

// Sink is the destination for one command's guest output. It writes ordinary
// output on to the run log, reduces carriage-return redraws to their final
// frame, reports the section markers it passes, and retains the most recent
// lines so a failure can show what led to it.
//
// A failed step that says only "see the log" trades one kind of unhelpfulness
// for another, so the tail exists to put the error itself back on the console
// while the bulk stays on disk.
type Sink struct {
	mu        sync.Mutex
	out       io.Writer
	onSection func(string)
	tail      []string
	tailBytes int
	capacity  int

	// logProgress holds the portion of an arriving chunk that contains a
	// carriage return through the line ending. Holding that run lets the log
	// receive only its final frame instead of every redraw.
	logProgress []byte

	// partial is the line still waiting for its newline, held to the byte
	// ceiling; dropped counts what the ceiling refused since this line last
	// started, so a cut line can report how much of it is missing.
	partial []byte
	dropped int
}

// NewSink returns a Sink writing to out. onSection may be nil. tailLines is
// how many recent lines Tail reports.
func NewSink(out io.Writer, onSection func(string), tailLines int) *Sink {
	if tailLines < 1 {
		tailLines = 1
	}
	return &Sink{out: out, onSection: onSection, capacity: tailLines}
}

// Write records the output and reports any complete lines it contains.
func (s *Sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeLog(p); err != nil {
		return 0, err
	}

	// A pipe delivers whatever was ready, so a line can span several writes
	// and is only interpreted once its newline arrives. Scanning the arriving
	// bytes rather than the whole accumulated line keeps the cost of a long
	// line linear in its length.
	rest := p
	for {
		index := bytes.IndexByte(rest, '\n')
		if index < 0 {
			break
		}
		s.grow(rest[:index])
		s.consumeLine(s.takeLine())
		rest = rest[index+1:]
	}
	s.grow(rest)
	return len(p), nil
}

// writeLog writes ordinary output immediately and holds a carriage-return run
// until its newline arrives. Progress tools commonly emit each frame in a
// separate write, so the held run can span any number of Write calls.
func (s *Sink) writeLog(p []byte) error {
	if s.out == nil {
		return nil
	}

	if len(s.logProgress) > 0 {
		if index := bytes.IndexByte(p, '\n'); index >= 0 {
			s.logProgress = append(s.logProgress, p[:index+1]...)
			if err := s.writeLogBytes(collapseProgressFrames(s.logProgress)); err != nil {
				return err
			}
			s.logProgress = s.logProgress[:0]
			p = p[index+1:]
		} else {
			s.logProgress = append(s.logProgress, p...)
			return nil
		}
	}

	for len(p) > 0 {
		newline := bytes.IndexByte(p, '\n')
		if newline < 0 {
			if bytes.IndexByte(p, '\r') >= 0 {
				s.logProgress = append(s.logProgress[:0], p...)
				return nil
			}
			return s.writeLogBytes(p)
		}

		line := p[:newline+1]
		if err := s.writeLogBytes(collapseProgressFrames(line)); err != nil {
			return err
		}
		p = p[newline+1:]
	}
	return nil
}

func (s *Sink) writeLogBytes(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	_, err := s.out.Write(p)
	return err
}

// collapseProgressFrames keeps the last redraw before a line ending. A CRLF
// ending is excluded from the redraw search and retained byte-for-byte.
func collapseProgressFrames(line []byte) []byte {
	contentEnd := len(line)
	if contentEnd > 0 && line[contentEnd-1] == '\n' {
		contentEnd--
		if contentEnd > 0 && line[contentEnd-1] == '\r' {
			contentEnd--
		}
	}
	if index := bytes.LastIndexByte(line[:contentEnd], '\r'); index >= 0 {
		collapsed := make([]byte, 0, len(line)-index-1)
		collapsed = append(collapsed, line[index+1:contentEnd]...)
		collapsed = append(collapsed, line[contentEnd:]...)
		return collapsed
	}
	return line
}

// grow adds the next piece of the current line, applying the redraw rule and
// the byte ceiling as the bytes arrive so neither a progress indicator nor a
// guest dumping a file can make this buffer large.
func (s *Sink) grow(b []byte) {
	if len(b) == 0 {
		return
	}

	// A carriage return held over from the previous write is resolved now that
	// what follows it is known: a newline makes it a line ending, anything else
	// makes it a redraw that erased the line so far.
	if n := len(s.partial); n > 0 && s.partial[n-1] == '\r' && b[0] != '\n' {
		s.partial = s.partial[:0]
		s.dropped = 0
	}

	// Only the segment after the last carriage return was ever visible. A
	// trailing one is left in place because it is still ambiguous.
	if index := bytes.LastIndexByte(b[:len(b)-1], '\r'); index >= 0 {
		s.partial = s.partial[:0]
		s.dropped = 0
		b = b[index+1:]
	}

	room := maxTailLineBytes - len(s.partial)
	if room < len(b) {
		if room > 0 {
			s.partial = append(s.partial, b[:room]...)
			s.dropped += len(b) - room
		} else {
			s.dropped += len(b)
		}
		return
	}
	s.partial = append(s.partial, b...)
}

// takeLine completes the current line and resets the accumulator for the next.
func (s *Sink) takeLine() string {
	line := boundLine(lastRedraw(string(s.partial)), s.dropped)
	s.partial = s.partial[:0]
	s.dropped = 0
	return line
}

func (s *Sink) consumeLine(line string) {
	s.appendTail(line)

	if s.onSection == nil {
		return
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, sectionMarker) || !strings.HasSuffix(trimmed, sectionMarker) {
		return
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, sectionMarker), sectionMarker))
	if inner == "" {
		return
	}
	s.onSection(inner)
}

// lastRedraw reduces a line to what it finally read.
//
// Two different things produce carriage returns here. Output that crossed a
// pseudo-terminal is CRLF-terminated, so the trailing one is part of the line
// ending and is simply removed. Any that remain came from tooling redrawing a
// progress indicator in place, where only the segment after the last one was
// ever visible; keeping the earlier segments makes a replayed tail overwrite
// itself in the reader's terminal.
func lastRedraw(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if index := strings.LastIndexByte(line, '\r'); index >= 0 {
		line = line[index+1:]
	}
	return line
}

// boundLine cuts a line to the ceiling and says how much it lost. A line that
// silently dropped its end would be worse than one that admits it: the reader
// would take the fragment for the whole message.
func boundLine(line string, dropped int) string {
	if dropped <= 0 {
		return line
	}
	notice := fmt.Sprintf("… (truncated, %d bytes omitted)", dropped)
	limit := maxTailLineBytes - len(notice)
	if limit < 0 {
		limit = 0
	}
	return trimToRune(line, limit) + notice
}

// trimToRune shortens s to at most limit bytes, ending on a character
// boundary. Cutting between the bytes of one character would replace it with
// the replacement glyph, which reads as corruption rather than as a cut.
func trimToRune(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

func (s *Sink) appendTail(line string) {
	s.tail = append(s.tail, line)
	s.tailBytes += len(line)
	// The newest output is the point of the tail, so both ceilings are enforced
	// by dropping from the front. One line always survives: boundLine has
	// already cut it to fit, so keeping it cannot exceed the byte ceiling by
	// more than that one line.
	for len(s.tail) > 1 && (len(s.tail) > s.capacity || s.tailBytes > maxTailBytes) {
		s.tailBytes -= len(s.tail[0])
		s.tail = s.tail[1:]
	}
}

// Tail returns the most recent lines, including a final line that never got
// its newline: a command killed mid-line still reports what it managed to say,
// and that fragment is often the error worth reading.
//
// What it returns is bounded in bytes as well as in lines, because it is
// replayed to the console and a line count alone bounds nothing.
func (s *Sink) Tail() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	lines := append([]string(nil), s.tail...)
	total := s.tailBytes
	if remainder := boundLine(lastRedraw(string(s.partial)), s.dropped); remainder != "" {
		lines = append(lines, remainder)
		total += len(remainder)
	}
	for len(lines) > 1 && (len(lines) > s.capacity || total > maxTailBytes) {
		total -= len(lines[0])
		lines = lines[1:]
	}
	return lines
}
