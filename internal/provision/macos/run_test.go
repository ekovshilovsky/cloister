package macos

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"cloister.io/internal/config"
	"cloister.io/internal/provision/report"
	"cloister.io/internal/vm"
)

type recordingStep struct {
	name    string
	out     bytes.Buffer
	outcome string
}

func (s *recordingStep) Writer() io.Writer   { return &s.out }
func (s *recordingStep) Done()               { s.outcome = "done" }
func (s *recordingStep) Fail()               { s.outcome = "fail" }
func (s *recordingStep) Warn(message string) { s.outcome = "warn" }

type recordingReporter struct{ steps []*recordingStep }

func (r *recordingReporter) Step(name string) report.Step {
	step := &recordingStep{name: name}
	r.steps = append(r.steps, step)
	return step
}

// TestRunReportsEveryStepAndRecordsItsOutput covers the two things the macOS
// sequence did not do: it named each step on the console but threw the guest
// output away, leaving a failed provision with nothing to read.
func TestRunReportsEveryStepAndRecordsItsOutput(t *testing.T) {
	backend := &vm.MockBackend{SSHCommandOut: "Homebrew installed 42 formulae\n"}
	steps := &recordingReporter{}
	engine := &Engine{Steps: steps}

	if err := engine.Run("mac", &config.Profile{}, backend); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(steps.steps) != len(PreflightSteps())+len(ProvisioningSteps())+len(HardeningSteps()) {
		t.Fatalf("reported %d steps, want one per provisioning step", len(steps.steps))
	}
	for _, step := range steps.steps {
		if step.outcome != "done" {
			t.Errorf("step %q outcome = %q, want %q", step.name, step.outcome, "done")
		}
		if !strings.Contains(step.out.String(), "Homebrew installed 42 formulae") {
			t.Errorf("step %q did not record its guest output, got %q", step.name, step.out.String())
		}
	}
}

// TestRunFailsTheStepThatFailed keeps a failed macOS provision pointing at the
// step that broke rather than at the sequence as a whole.
func TestRunFailsTheStepThatFailed(t *testing.T) {
	backend := &vm.MockBackend{SSHCommandErr: errSyntheticFailure}
	steps := &recordingReporter{}
	engine := &Engine{Steps: steps}

	if err := engine.Run("mac", &config.Profile{}, backend); err == nil {
		t.Fatal("Run() error = nil, want the step failure")
	}

	if len(steps.steps) != 1 {
		t.Fatalf("reported %d steps, want the run to stop at the first failure", len(steps.steps))
	}
	if steps.steps[0].outcome != "fail" {
		t.Errorf("step %q outcome = %q, want %q", steps.steps[0].name, steps.steps[0].outcome, "fail")
	}
}

// syntheticFailure is the error a failing backend returns in tests. It carries
// no diagnostic value beyond being non-nil.
type syntheticFailure struct{}

func (syntheticFailure) Error() string { return "synthetic guest failure" }

var errSyntheticFailure = syntheticFailure{}
