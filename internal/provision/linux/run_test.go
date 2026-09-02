package linux

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"cloister.io/internal/config"
	"cloister.io/internal/provision/report"
	"cloister.io/internal/vm"
)

// recordingStep stands in for the console: it keeps what a step was told so a
// test can assert on the report the user would have seen and on where the
// guest output went.
type recordingStep struct {
	name    string
	out     bytes.Buffer
	outcome string
	message string
}

func (s *recordingStep) Writer() io.Writer   { return &s.out }
func (s *recordingStep) Done()               { s.outcome = "done" }
func (s *recordingStep) Fail()               { s.outcome = "fail" }
func (s *recordingStep) Warn(message string) { s.outcome, s.message = "warn", message }

type recordingReporter struct{ steps []*recordingStep }

func (r *recordingReporter) Step(name string) report.Step {
	step := &recordingStep{name: name}
	r.steps = append(r.steps, step)
	return step
}

func (r *recordingReporter) named(name string) *recordingStep {
	for _, step := range r.steps {
		if step.name == name {
			return step
		}
	}
	return nil
}

func (r *recordingReporter) names() []string {
	names := make([]string, 0, len(r.steps))
	for _, step := range r.steps {
		names = append(names, step.name)
	}
	return names
}

// TestRunReportsAStepPerUnitOfWork covers the reason this exists: the console
// gets one line per unit of work, and the guest output that used to fill it
// goes to the destination the caller supplied.
func TestRunReportsAStepPerUnitOfWork(t *testing.T) {
	backend := &vm.MockBackend{SSHScriptOut: "=== Installing Node.js via NVM ===\nreams of apt output\n"}
	steps := &recordingReporter{}
	engine := &Engine{Steps: steps}

	if err := engine.Run("dev", &config.Profile{Stacks: []string{"dotnet"}}, backend); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, want := range []string{"Base tools", "dotnet stack"} {
		if steps.named(want) == nil {
			t.Errorf("no %q step reported; got %v", want, steps.names())
		}
	}

	base := steps.named("Base tools")
	if base == nil {
		t.Fatalf("no base tools step reported; got %v", steps.names())
	}
	if !strings.Contains(base.out.String(), "reams of apt output") {
		t.Errorf("base tools guest output did not reach the step, got %q", base.out.String())
	}

	// A step left in flight is a step whose outcome the reader never learns.
	for _, step := range steps.steps {
		if step.outcome == "" {
			t.Errorf("step %q never settled", step.name)
		}
	}
}

// TestRunFailsTheStepThatFailed keeps the failing step identifiable: a run that
// reports every step as done and then returns an error tells the reader an
// error happened but not where.
func TestRunFailsTheStepThatFailed(t *testing.T) {
	backend := &failingScriptBackend{failOn: "=== Installing .NET"}
	steps := &recordingReporter{}
	engine := &Engine{Steps: steps}

	err := engine.Run("dev", &config.Profile{Stacks: []string{"dotnet"}}, backend)
	if err == nil {
		t.Fatal("Run() error = nil, want the stack failure")
	}

	stack := steps.named("dotnet stack")
	if stack == nil {
		t.Fatalf("no dotnet stack step reported; got %v", steps.names())
	}
	if stack.outcome != "fail" {
		t.Errorf("dotnet stack step outcome = %q, want %q", stack.outcome, "fail")
	}
	if base := steps.named("Base tools"); base == nil || base.outcome != "done" {
		t.Errorf("base tools step should have completed before the failure, got %+v", base)
	}
}

func TestRunReportsPluginSyncFailureAndCapturesItsOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	backend := &failingScriptBackend{failOn: "cat > ~/.claude/.claude.json"}
	steps := &recordingReporter{}
	engine := &Engine{Steps: steps}

	if err := engine.Run("dev", &config.Profile{}, backend); err != nil {
		t.Fatalf("Run() error = %v; plugin sync is a reported non-fatal step", err)
	}

	plugin := steps.named("Plugin configuration")
	if plugin == nil {
		t.Fatalf("no plugin configuration step reported; got %v", steps.names())
	}
	if plugin.outcome != "warn" {
		t.Errorf("plugin configuration outcome = %q, want %q", plugin.outcome, "warn")
	}
	if !strings.Contains(plugin.message, "writing default .claude.json") {
		t.Errorf("plugin warning = %q, want the failed operation", plugin.message)
	}
	if !strings.Contains(plugin.out.String(), "Unable to locate package") {
		t.Errorf("plugin guest output bypassed its step writer: %q", plugin.out.String())
	}
}

// failingScriptBackend fails only the script whose body carries a marker, so a
// test can put one step of a sequence into failure without disturbing the rest.
type failingScriptBackend struct {
	vm.MockBackend
	failOn string
}

func (b *failingScriptBackend) SSHScriptTo(profile, script string, out io.Writer) (string, error) {
	if strings.Contains(script, b.failOn) {
		io.WriteString(out, "E: Unable to locate package dotnet-sdk-10\n")
		return "", errSyntheticFailure
	}
	return b.MockBackend.SSHScriptTo(profile, script, out)
}

// syntheticFailure is the error a failing backend returns in tests. It carries
// no diagnostic value beyond being non-nil.
type syntheticFailure struct{}

func (syntheticFailure) Error() string { return "synthetic guest failure" }

var errSyntheticFailure = syntheticFailure{}
