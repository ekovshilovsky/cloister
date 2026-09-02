package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestQuietSessionKeepsGuestOutputOffTheConsole pins what the session is for:
// the console carries the step, the run log carries the output it produced.
func TestQuietSessionKeepsGuestOutputOffTheConsole(t *testing.T) {
	var console, log bytes.Buffer
	session := newProvisionSession(&console, &log, false)
	defer session.Close()

	step := session.Step("Base tools")
	if _, err := io.WriteString(step.Writer(), "reams of apt output\n"); err != nil {
		t.Fatalf("writing guest output: %v", err)
	}
	step.Done()

	if strings.Contains(console.String(), "reams of apt output") {
		t.Errorf("guest output reached the console: %q", console.String())
	}
	if !strings.Contains(log.String(), "reams of apt output") {
		t.Errorf("guest output did not reach the run log: %q", log.String())
	}
	if !strings.Contains(console.String(), "Base tools") {
		t.Errorf("console does not report the step: %q", console.String())
	}
}

// TestSessionWithoutARunLogStillReports covers the degraded case: a session
// that could not open its log reports progress rather than refusing to run.
func TestSessionWithoutARunLogStillReports(t *testing.T) {
	var console bytes.Buffer
	session := newProvisionSession(&console, nil, false)
	defer session.Close()

	step := session.Step("Base tools")
	if _, err := io.WriteString(step.Writer(), "reams of apt output\n"); err != nil {
		t.Fatalf("writing guest output: %v", err)
	}
	step.Done()

	if !strings.Contains(console.String(), "Base tools") {
		t.Errorf("console does not report the step: %q", console.String())
	}
}
