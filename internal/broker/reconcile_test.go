package broker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type reconciliationStep struct {
	command string
	output  string
	err     error
}

type reconciliationRunner struct {
	steps []reconciliationStep
	calls []string
}

func (r *reconciliationRunner) Run(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	r.calls = append(r.calls, command)
	if len(r.steps) == 0 {
		return nil, errors.New("unexpected command: " + command)
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	if command != step.command {
		return nil, errors.New("got command " + command + ", want " + step.command)
	}
	return []byte(step.output), step.err
}

func reconciliationSpec(profile, projectID string) SessionSpec {
	return SessionSpec{
		Profile:   profile,
		ProjectID: projectID,
		Name:      "cloister-" + sanitize(profile) + "-" + projectID,
	}
}

func mutagenStatus(name, state string) string {
	return "Name: " + name + "\nConflicts: 0\nStatus: " + state + "\n"
}

func mutagenListRecord(name string) string {
	return "--------------------------------------------------------------------------------\n" +
		"Name: " + name + "\n" +
		"Identifier: sync_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789\n" +
		"Alpha:\n\tURL: /tmp/example\n\tConnected: Yes\n" +
		"Beta:\n\tURL: ssh://example/~/workspaces/example\n\tConnected: Yes\n" +
		"Conflicts: 0\nStatus: Watching for changes\n"
}

func TestMutagenReconcileProfileFlushesCleanActiveObsoleteSessionBeforeTermination(t *testing.T) {
	desired := reconciliationSpec("local-dev", "111111111111111111111111")
	obsolete := reconciliationSpec("local-dev", "222222222222222222222222")
	otherProfile := reconciliationSpec("other", "333333333333333333333333")
	prefixCollision := reconciliationSpec("local-dev-extra", "444444444444444444444444")
	listOutput := "Starting Mutagen daemon...\n" +
		mutagenListRecord(desired.Name) +
		mutagenListRecord(obsolete.Name) +
		mutagenListRecord(otherProfile.Name) +
		mutagenListRecord(prefixCollision.Name) +
		mutagenListRecord("manually-managed")
	runner := &reconciliationRunner{steps: []reconciliationStep{
		{command: "sync list", output: listOutput},
		{command: "sync list --long " + obsolete.Name, output: mutagenStatus(obsolete.Name, "Watching for changes")},
		{command: "sync flush " + obsolete.Name, output: "ok\n"},
		{command: "sync list --long " + obsolete.Name, output: mutagenStatus(obsolete.Name, "Watching for changes")},
		{command: "sync list --long " + obsolete.Name, output: mutagenStatus(obsolete.Name, "Watching for changes")},
		{command: "sync terminate " + obsolete.Name, output: "ok\n"},
	}}
	dataDir := t.TempDir()
	policyPath := filepath.Join(dataDir, "cloister-policies", obsolete.Name+".sha256")
	if err := os.MkdirAll(filepath.Dir(policyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, []byte("fingerprint\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutagen := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: dataDir}

	if err := mutagen.ReconcileProfile(context.Background(), "local-dev", []SessionSpec{desired}); err != nil {
		t.Fatal(err)
	}
	if len(runner.steps) != 0 {
		t.Fatalf("unused command steps: %#v", runner.steps)
	}
	if _, err := os.Stat(policyPath); !os.IsNotExist(err) {
		t.Fatalf("obsolete policy fingerprint still exists: %v", err)
	}
	for _, preserved := range []string{desired.Name, otherProfile.Name, prefixCollision.Name, "manually-managed"} {
		for _, call := range runner.calls[1:] {
			if strings.HasSuffix(call, " "+preserved) {
				t.Fatalf("preserved session %q was touched: %v", preserved, runner.calls)
			}
		}
	}
}

func TestMutagenReconcileProfileTerminatesCleanPausedSessionWithoutFlush(t *testing.T) {
	obsolete := reconciliationSpec("local-dev", "222222222222222222222222")
	runner := &reconciliationRunner{steps: []reconciliationStep{
		{command: "sync list", output: "Name: " + obsolete.Name + "\n"},
		{command: "sync list --long " + obsolete.Name, output: realMutagenPausedSessionOutput},
		{command: "sync list --long " + obsolete.Name, output: realMutagenPausedSessionOutput},
		{command: "sync terminate " + obsolete.Name, output: "ok\n"},
	}}
	mutagen := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: t.TempDir()}

	if err := mutagen.ReconcileProfile(context.Background(), "local-dev", nil); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "sync flush ") {
			t.Fatalf("paused session was flushed: %v", runner.calls)
		}
	}
}

func TestMutagenReconcileProfileWithNoSessionsIsNoOp(t *testing.T) {
	runner := &reconciliationRunner{steps: []reconciliationStep{
		{command: "sync list", output: "Starting Mutagen daemon...\nNo sessions found\n"},
	}}
	mutagen := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: t.TempDir()}

	if err := mutagen.ReconcileProfile(context.Background(), "local-dev", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.calls, []string{"sync list"}) {
		t.Fatalf("calls = %v", runner.calls)
	}
}

func TestMutagenReconcileProfileFailsClosed(t *testing.T) {
	obsolete := reconciliationSpec("local-dev", "222222222222222222222222")
	clean := mutagenStatus(obsolete.Name, "Watching for changes")
	tests := []struct {
		name  string
		steps []reconciliationStep
	}{
		{
			name: "conflict",
			steps: []reconciliationStep{
				{command: "sync list", output: "Name: " + obsolete.Name + "\n"},
				{command: "sync list --long " + obsolete.Name, output: "Name: " + obsolete.Name + "\nConflicts: 1\nStatus: Watching for changes\n"},
			},
		},
		{
			name: "endpoint problem",
			steps: []reconciliationStep{
				{command: "sync list", output: "Name: " + obsolete.Name + "\n"},
				{command: "sync list --long " + obsolete.Name, output: "Name: " + obsolete.Name + "\nBeta:\n Connected: No\nConflicts: 0\nStatus: Watching for changes\n"},
			},
		},
		{
			name: "unknown status",
			steps: []reconciliationStep{
				{command: "sync list", output: "Name: " + obsolete.Name + "\n"},
				{command: "sync list --long " + obsolete.Name, output: "Name: " + obsolete.Name + "\nStatus: Synchronizing\n"},
			},
		},
		{
			name: "duplicate list record",
			steps: []reconciliationStep{
				{command: "sync list", output: "Name: " + obsolete.Name + "\nName: " + obsolete.Name + "\n"},
			},
		},
		{
			name: "malformed managed list record",
			steps: []reconciliationStep{
				{command: "sync list", output: "Name: cloister-local-dev-not-an-id\n"},
			},
		},
		{
			name: "flush failure",
			steps: []reconciliationStep{
				{command: "sync list", output: "Name: " + obsolete.Name + "\n"},
				{command: "sync list --long " + obsolete.Name, output: clean},
				{command: "sync flush " + obsolete.Name, output: "flush failed\n", err: runnerExitError(1)},
			},
		},
		{
			name: "terminate failure",
			steps: []reconciliationStep{
				{command: "sync list", output: "Name: " + obsolete.Name + "\n"},
				{command: "sync list --long " + obsolete.Name, output: "Name: " + obsolete.Name + "\nConflicts: 0\nStatus: Paused\n"},
				{command: "sync list --long " + obsolete.Name, output: "Name: " + obsolete.Name + "\nConflicts: 0\nStatus: Paused\n"},
				{command: "sync terminate " + obsolete.Name, output: "terminate failed\n", err: runnerExitError(1)},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &reconciliationRunner{steps: append([]reconciliationStep(nil), test.steps...)}
			mutagen := &Mutagen{Binary: "mutagen", Runner: runner, DataDir: t.TempDir()}
			if err := mutagen.ReconcileProfile(context.Background(), "local-dev", nil); err == nil {
				t.Fatal("ReconcileProfile() error = nil")
			}
			for _, call := range runner.calls {
				if strings.HasPrefix(call, "sync create ") || strings.HasPrefix(call, "sync resume ") {
					t.Fatalf("activation continued after reconciliation failure: %v", runner.calls)
				}
			}
		})
	}
}
