package linux

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestRunExecutesGlobalThenProfileProvisioningHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".cloister")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	global := "#!/bin/sh\necho global-hook-marker\n"
	profile := "#!/bin/sh\necho profile-hook-marker\n"
	if err := os.WriteFile(filepath.Join(configDir, "provision.sh"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "provision-dev.sh"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "provision-other.sh"), []byte("echo wrong-profile\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	backend := &vm.MockBackend{}
	steps := &recordingReporter{}
	engine := &Engine{Steps: steps}
	if err := engine.Run("dev", &config.Profile{}, backend); err != nil {
		t.Fatal(err)
	}

	var hooks []string
	for _, call := range backend.SSHScriptCalls {
		if strings.Contains(call.Script, "hook-marker") || strings.Contains(call.Script, "wrong-profile") {
			hooks = append(hooks, call.Script)
		}
	}
	if len(hooks) != 2 || hooks[0] != global || hooks[1] != profile {
		t.Fatalf("executed hooks = %#v, want global then matching profile", hooks)
	}
	for _, name := range []string{"Global provisioning hook", "dev provisioning hook"} {
		step := steps.named(name)
		if step == nil || step.outcome != "done" {
			t.Errorf("step %q = %+v, want completed", name, step)
		}
	}
}

func TestRunStopsWhenCustomProvisioningHookFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".cloister")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "provision.sh"), []byte("echo fail-hook-marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "provision-dev.sh"), []byte("echo must-not-run\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	backend := &failingScriptBackend{failOn: "fail-hook-marker"}
	steps := &recordingReporter{}
	err := (&Engine{Steps: steps}).Run("dev", &config.Profile{}, backend)
	if err == nil || !strings.Contains(err.Error(), "global provisioning hook") {
		t.Fatalf("Run() error = %v, want the failed global hook", err)
	}
	global := steps.named("Global provisioning hook")
	if global == nil || global.outcome != "fail" {
		t.Fatalf("global hook step = %+v, want failed", global)
	}
	for _, call := range backend.SSHScriptCalls {
		if strings.Contains(call.Script, "must-not-run") {
			t.Fatalf("profile hook ran after global failure: %#v", backend.SSHScriptCalls)
		}
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

// A template deploy is a guest command like any other, and a failed one is
// exactly when its output is worth having. Discarding it leaves the failure
// tail empty and the run log silent about the step that stopped the run.
func TestRunSendsTemplateDeployOutputToItsStep(t *testing.T) {
	backend := &vm.MockBackend{SSHCommandOut: "bash: cat: cannot create ~/.bashrc: Read-only file system\n"}
	steps := &recordingReporter{}
	engine := &Engine{Steps: steps}

	if err := engine.Run("dev", &config.Profile{}, backend); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	shell := steps.named("Shell configuration")
	if shell == nil {
		t.Fatalf("no shell configuration step reported; got %v", steps.names())
	}
	if !strings.Contains(shell.out.String(), "Read-only file system") {
		t.Errorf("bashrc deploy output did not reach its step, got %q", shell.out.String())
	}
}

// The auditor's reproduction: a bashrc deploy that fails reports the step that
// broke and nothing at all about why.
func TestFailedBashrcDeployReportsWhatTheGuestSaid(t *testing.T) {
	backend := &vm.MockBackend{
		SSHCommandOut: "bash: line 1: /home/x/.bashrc: Permission denied\n",
		SSHCommandErr: errSyntheticDeployFailure,
	}
	steps := &recordingReporter{}
	engine := &Engine{Steps: steps}

	err := engine.Run("dev", &config.Profile{}, backend)
	if err == nil {
		t.Fatal("Run() succeeded; want the bashrc deploy failure")
	}

	shell := steps.named("Shell configuration")
	if shell == nil {
		t.Fatalf("no shell configuration step reported; got %v", steps.names())
	}
	if shell.outcome != "fail" {
		t.Errorf("shell configuration outcome = %q, want fail", shell.outcome)
	}
	if shell.out.Len() == 0 {
		t.Errorf("failed bashrc deploy reported err=%v with an empty step output", err)
	}
}

// DeployGitConfig deploys a template too, so it has the same obligation.
func TestDeployGitConfigSendsGuestOutputToOut(t *testing.T) {
	if !hostHasGitIdentity() {
		t.Skip("host git identity is not configured; DeployGitConfig cannot render")
	}
	backend := &vm.MockBackend{SSHCommandOut: "cat: write error: No space left on device\n"}
	var out bytes.Buffer
	engine := &Engine{Out: &out}

	if err := engine.DeployGitConfig("dev", &config.Profile{}, backend); err != nil {
		t.Fatalf("DeployGitConfig() error = %v", err)
	}
	if !strings.Contains(out.String(), "No space left on device") {
		t.Errorf("gitconfig deploy output did not reach Out, got %q", out.String())
	}
}

func hostHasGitIdentity() bool {
	data := readHostGitConfig()
	return data.GitName != "" && data.GitEmail != ""
}

var errSyntheticDeployFailure = errors.New("exit status 1")
