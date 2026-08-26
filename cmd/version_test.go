package cmd

import (
	"fmt"
	"io"
	"os"
	"testing"
)

// runRootCapturingStreams executes the root command with the given arguments
// while os.Stdout and os.Stderr are replaced by pipes, returning what each
// stream received. The process streams are captured rather than Cobra's
// SetOut/SetErr writers because setting an output writer changes which stream
// Cobra's Print helpers target, which would hide the very defect these tests
// guard against.
func runRootCapturingStreams(t *testing.T, args ...string) (stdout string, stderr string) {
	t.Helper()

	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdout pipe: %v", err)
	}
	errReader, errWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stderr pipe: %v", err)
	}

	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outWriter, errWriter

	outCh := readAsync(outReader)
	errCh := readAsync(errReader)

	rootCmd.SetArgs(args)
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	execErr := rootCmd.Execute()

	os.Stdout, os.Stderr = origStdout, origStderr
	outWriter.Close()
	errWriter.Close()

	stdout, stderr = <-outCh, <-errCh
	if execErr != nil {
		t.Fatalf("executing %v: %v (stdout %q, stderr %q)", args, execErr, stdout, stderr)
	}
	return stdout, stderr
}

// readAsync drains r in the background so a command writing more than the pipe
// buffer holds cannot block.
func readAsync(r *os.File) <-chan string {
	ch := make(chan string, 1)
	go func() {
		defer r.Close()
		data, _ := io.ReadAll(r)
		ch <- string(data)
	}()
	return ch
}

// TestVersionCommandWritesToStdout guards the contract that consumers which
// capture only stdout, such as the Homebrew formula test running
// "cloister version", receive the version string. Cobra's cmd.Printf writes to
// stderr unless an output writer is set, which leaves those consumers with
// empty output.
func TestVersionCommandWritesToStdout(t *testing.T) {
	stdout, stderr := runRootCapturingStreams(t, "version")

	want := fmt.Sprintf("cloister %s\n", Version)
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// TestRootVersionFlagWritesToStdout verifies the root "--version" flag shares
// the same stdout contract as the version subcommand.
func TestRootVersionFlagWritesToStdout(t *testing.T) {
	stdout, stderr := runRootCapturingStreams(t, "--version")

	want := fmt.Sprintf("cloister version %s\n", Version)
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	// Reset the persistent version flag so a later root execution in another
	// test is not short-circuited into version output.
	if flag := rootCmd.Flags().Lookup("version"); flag != nil {
		if err := flag.Value.Set("false"); err != nil {
			t.Fatalf("resetting version flag: %v", err)
		}
		flag.Changed = false
	}
}
