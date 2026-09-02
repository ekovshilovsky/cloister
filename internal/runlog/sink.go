// Proprietary and confidential. All rights reserved.

package runlog

import (
	"io"
	"strings"
	"sync"
)

// sectionMarker is the banner the provisioning scripts already print around
// each unit of work, for example:
//
//	=== Installing op-forward (1Password CLI forwarding) ===
//
// Reading them is what lets a step in the console name the sub-step actually
// running, without the scripts having to learn anything new.
const sectionMarker = "==="

// Sink is the destination for one command's guest output. It writes every byte
// on to the run log, reports the section markers it passes, and retains the
// most recent lines so a failure can show what led to it.
//
// A failed step that says only "see the log" trades one kind of unhelpfulness
// for another, so the tail exists to put the error itself back on the console
// while the bulk stays on disk.
type Sink struct {
	mu        sync.Mutex
	out       io.Writer
	onSection func(string)
	tail      []string
	capacity  int
	partial   strings.Builder
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

	// The log gets the bytes exactly as they arrived; only the interpretation
	// below works on reassembled lines.
	if s.out != nil {
		if _, err := s.out.Write(p); err != nil {
			return 0, err
		}
	}

	// A pipe delivers whatever was ready, so a line can span several writes
	// and is only interpreted once its newline arrives.
	s.partial.Write(p)
	buffered := s.partial.String()
	for {
		index := strings.IndexByte(buffered, '\n')
		if index < 0 {
			break
		}
		s.consumeLine(buffered[:index])
		buffered = buffered[index+1:]
	}
	s.partial.Reset()
	s.partial.WriteString(buffered)
	return len(p), nil
}

func (s *Sink) consumeLine(line string) {
	line = lastRedraw(line)
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

func (s *Sink) appendTail(line string) {
	s.tail = append(s.tail, line)
	if len(s.tail) > s.capacity {
		s.tail = s.tail[len(s.tail)-s.capacity:]
	}
}

// Tail returns the most recent lines, including a final line that never got
// its newline: a command killed mid-line still reports what it managed to say,
// and that fragment is often the error worth reading.
func (s *Sink) Tail() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	lines := append([]string(nil), s.tail...)
	if remainder := lastRedraw(s.partial.String()); remainder != "" {
		lines = append(lines, remainder)
	}
	if len(lines) > s.capacity {
		lines = lines[len(lines)-s.capacity:]
	}
	return lines
}
