package scan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateRoundTripPermissionsAndAtomicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "scan.json")
	state := validTestState(t)
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}

	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode = %o, want 700", got)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := before.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %o, want 600", got)
	}

	state.Proposal.Generator = "replacement"
	state.ProposalDigest, err = ProposalDigest(state.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("save did not atomically replace the target inode")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".scan.json-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}

	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Proposal.Generator != "replacement" {
		t.Fatalf("loaded replacement = %#v", loaded)
	}
	if loaded.ContentFingerprint != "content" {
		t.Fatalf("content fingerprint = %q", loaded.ContentFingerprint)
	}
}

func TestSaveStateDoesNotMutateCallerCollections(t *testing.T) {
	state := validTestState(t)
	state.Proposal.Projects = []Project{
		{ID: "z", Path: "z", Kind: ProjectLocal},
		{ID: "a", Path: "a", Kind: ProjectShared},
	}
	state.ProjectMappings = nil
	for _, project := range state.Proposal.Projects {
		root := filepath.Join(state.SourceRoot, project.Path)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		state.ProjectMappings = append(state.ProjectMappings, ProjectMapping{
			ProjectID: project.ID, PortablePath: project.Path, PhysicalRoot: root,
		})
	}
	state.Proposal.Policy.ProjectIgnore = map[string][]string{"z": {"z", "a"}}
	state.ProposalDigest, _ = ProposalDigest(state.Proposal)

	if err := SaveState(filepath.Join(t.TempDir(), "scan.json"), state); err != nil {
		t.Fatal(err)
	}
	if state.Proposal.Projects[0].ID != "z" || state.Proposal.Policy.ProjectIgnore["z"][0] != "z" {
		t.Fatalf("SaveState mutated caller collections: %#v", state.Proposal)
	}
}

func TestLoadStateRejectsUnknownVersionsAndMalformedInput(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"new format":      `{"formatVersion":2,"proposal":{}}`,
		"old format":      `{"formatVersion":0,"proposal":{}}`,
		"new schema":      stateJSONWithSchema(t, CurrentSchemaVersion+1),
		"old schema":      stateJSONWithSchema(t, CurrentSchemaVersion-1),
		"missing adapter": stateJSONWithoutAdapter(t),
		"incomplete":      `{"formatVersion":1,"proposal":{"schemaVersion":1}}`,
		"malformed":       `{`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadState(path)
			if err == nil {
				t.Fatal("LoadState succeeded unexpectedly")
			}
			if strings.HasPrefix(name, "old ") && !strings.Contains(err.Error(), "missing migration") {
				t.Fatalf("old version error = %v", err)
			}
		})
	}
}

func TestSaveStateRejectsUnsafeProfilesAndMappings(t *testing.T) {
	for _, profile := range []string{`bad\name`, "bad/name", "bad\x00name"} {
		state := validTestState(t)
		state.Profile = profile
		if err := SaveState(filepath.Join(t.TempDir(), "state.json"), state); err == nil {
			t.Fatalf("unsafe profile %q was accepted", profile)
		}
	}

	state := validTestState(t)
	state.ProjectMappings = append(state.ProjectMappings, state.ProjectMappings[0])
	if err := SaveState(filepath.Join(t.TempDir(), "state.json"), state); err == nil {
		t.Fatal("duplicate mapping was accepted")
	}

	state = validTestState(t)
	state.ProjectMappings[0].ProjectID = `bad\id`
	if err := SaveState(filepath.Join(t.TempDir(), "state.json"), state); err == nil {
		t.Fatal("backslash mapping ID was accepted")
	}
}

func TestStateRoundTripPreservesDecisionDerivedExclusions(t *testing.T) {
	state := validTestState(t)
	state.Proposal.Findings = []Finding{{
		Class: ClassDatabase, ProjectID: "project", Path: "local.db", Size: 10,
		Reason: "database", Recommendation: RecommendationExclude, Decision: DecisionExclude,
	}}
	RebuildExclusions(&state.Proposal)
	state.ProposalDigest, _ = ProposalDigest(state.Proposal)
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Proposal.Exclusions) != 1 || loaded.Proposal.Exclusions[0].Path != "local.db" {
		t.Fatalf("round-trip exclusions = %#v", loaded.Proposal.Exclusions)
	}
}

func TestLoadStateRejectsMissingContentFingerprint(t *testing.T) {
	state := validTestState(t)
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "contentFingerprint")
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil || !strings.Contains(err.Error(), "fingerprints") {
		t.Fatalf("LoadState() error = %v", err)
	}
}

func stateJSONWithoutAdapter(t *testing.T) string {
	t.Helper()
	var state map[string]any
	if err := json.Unmarshal([]byte(stateJSONWithSchema(t, CurrentSchemaVersion)), &state); err != nil {
		t.Fatal(err)
	}
	proposal := state["proposal"].(map[string]any)
	source := proposal["source"].(map[string]any)
	delete(source, "adapter")
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func stateJSONWithSchema(t *testing.T, version int) string {
	t.Helper()
	state := validTestState(t)
	state.Proposal.SchemaVersion = version
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func validTestState(t *testing.T) StateEnvelope {
	t.Helper()
	proposal := validTestProposal()
	sourceRoot := t.TempDir()
	sourceRoot, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(sourceRoot, "project")
	if err := os.Mkdir(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := ProposalDigest(proposal)
	if err != nil {
		t.Fatal(err)
	}
	return StateEnvelope{
		FormatVersion:      CurrentFormatVersion,
		Profile:            "test",
		SourceRoot:         sourceRoot,
		ConfigFingerprint:  "config",
		SourceFingerprint:  "source",
		ContentFingerprint: "content",
		ProposalDigest:     digest,
		ProjectMappings: []ProjectMapping{{
			ProjectID: "project", PortablePath: "project", PhysicalRoot: projectRoot,
		}},
		Proposal: proposal,
	}
}
