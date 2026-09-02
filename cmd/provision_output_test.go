package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestQuietSessionKeepsGuestOutputOffTheConsole pins the default: the console
// carries the step, the run log carries the output the step produced.
func TestQuietSessionKeepsGuestOutputOffTheConsole(t *testing.T) {
	var console, log bytes.Buffer
	session := newProvisionSession(&console, &log, false, false)
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

// TestVerboseSessionStreamsGuestOutputToBothDestinations covers the debugging
// case --verbose exists for: watching a provision as it happens without giving
// up the record of it.
func TestVerboseSessionStreamsGuestOutputToBothDestinations(t *testing.T) {
	var console, log bytes.Buffer
	session := newProvisionSession(&console, &log, true, true)
	defer session.Close()

	step := session.Step("Base tools")
	if _, err := io.WriteString(step.Writer(), "reams of apt output\n"); err != nil {
		t.Fatalf("writing guest output: %v", err)
	}
	step.Done()

	if !strings.Contains(console.String(), "reams of apt output") {
		t.Errorf("guest output did not reach the console: %q", console.String())
	}
	if !strings.Contains(log.String(), "reams of apt output") {
		t.Errorf("guest output did not reach the run log: %q", log.String())
	}
	// A spinner rewrites the line it shares with the stream, so verbose renders
	// plainly even on a terminal.
	if strings.ContainsAny(console.String(), "\r\x1b") {
		t.Errorf("verbose console carries cursor control: %q", console.String())
	}
}

// TestSessionWithoutARunLogStillReports covers the degraded case: a session
// that could not open its log reports progress rather than refusing to run.
func TestSessionWithoutARunLogStillReports(t *testing.T) {
	var console bytes.Buffer
	session := newProvisionSession(&console, nil, false, true)
	defer session.Close()

	step := session.Step("Base tools")
	if _, err := io.WriteString(step.Writer(), "reams of apt output\n"); err != nil {
		t.Fatalf("writing guest output: %v", err)
	}
	step.Done()

	if !strings.Contains(console.String(), "Base tools") {
		t.Errorf("console does not report the step: %q", console.String())
	}
	if !strings.Contains(console.String(), "reams of apt output") {
		t.Errorf("verbose guest output did not reach the console: %q", console.String())
	}
}

// TestProvisioningCommandsOfferVerbose keeps the flag on every command whose
// guest output now goes to the run log: without it there is no way to watch a
// provision as it runs.
func TestProvisioningCommandsOfferVerbose(t *testing.T) {
	for _, command := range []*cobra.Command{createCmd, rebuildCmd, addStackCmd, repairCmd} {
		if command.Flags().Lookup("verbose") == nil {
			t.Errorf("%q has no --verbose flag", command.Name())
		}
	}
}
