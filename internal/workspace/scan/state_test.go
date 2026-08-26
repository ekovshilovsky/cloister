package scan

import (
	"encoding/json"
	"fmt"
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
	if loaded.ProjectFingerprints["project"] != "project-content" {
		t.Fatalf("project fingerprints = %#v", loaded.ProjectFingerprints)
	}
}

func TestSaveStateDoesNotMutateCallerCollections(t *testing.T) {
	state := validTestState(t)
	state.Proposal.Projects = []Project{
		includedTestProject("z", "z", ProjectLocal),
		includedTestProject("a", "a", ProjectShared),
	}
	state.ProjectMappings = nil
	state.ProjectFingerprints = map[string]string{}
	for _, project := range state.Proposal.Projects {
		root := filepath.Join(state.SourceRoot, project.Path)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		state.ProjectMappings = append(state.ProjectMappings, ProjectMapping{
			ProjectID: project.ID, PortablePath: project.Path, PhysicalRoot: root,
		})
		state.ProjectFingerprints[project.ID] = "content-" + project.ID
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
		"new format":      fmt.Sprintf(`{"formatVersion":%d,"proposal":{}}`, CurrentFormatVersion+1),
		"old format":      `{"formatVersion":0,"proposal":{}}`,
		"new schema":      stateJSONWithSchema(t, CurrentSchemaVersion+1),
		"old schema":      stateJSONWithSchema(t, CurrentSchemaVersion-1),
		"missing adapter": stateJSONWithoutAdapter(t),
		"incomplete":      fmt.Sprintf(`{"formatVersion":%d,"proposal":{"schemaVersion":%d}}`, CurrentFormatVersion, CurrentSchemaVersion),
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
			if name == "old format" && !strings.Contains(err.Error(), "missing migration") {
				t.Fatalf("old version error = %v", err)
			}
			if name == "old schema" && !strings.Contains(err.Error(), "re-run cloister workspace scan") {
				t.Fatalf("old schema error = %v", err)
			}
		})
	}
}

func TestLoadStateRequiresRescanForVersionOneState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"formatVersion":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadState(path)
	const want = "workspace discovery state format version 1 is obsolete; re-run cloister workspace scan"
	if err == nil || err.Error() != want {
		t.Fatalf("LoadState() error = %v, want %q", err, want)
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

func TestStateAcceptsSourceRootProjectMapping(t *testing.T) {
	state := validTestState(t)
	state.Proposal.Projects = []Project{includedTestProject(".", ".", ProjectRepository)}
	state.ProjectMappings = []ProjectMapping{{
		ProjectID: ".", PortablePath: ".", PhysicalRoot: state.SourceRoot,
	}}
	state.ProjectFingerprints = map[string]string{".": "root-content"}
	state.ProposalDigest, _ = ProposalDigest(state.Proposal)
	if err := SaveState(filepath.Join(t.TempDir(), "state.json"), state); err != nil {
		t.Fatalf("source-root project mapping was rejected: %v", err)
	}
}

func TestReviewedStateRejectsUnresolvedProjectCandidate(t *testing.T) {
	state := validTestState(t)
	state.Reviewed = true
	state.Proposal.Projects[0].Recommendation = RecommendationReview
	state.Proposal.Projects[0].Decision = DecisionReview
	state.ProposalDigest, _ = ProposalDigest(state.Proposal)
	err := SaveState(filepath.Join(t.TempDir(), "state.json"), state)
	if err == nil || !strings.Contains(err.Error(), "unresolved project decisions") {
		t.Fatalf("SaveState() error = %v", err)
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

func TestLoadStateRejectsMissingProjectFingerprints(t *testing.T) {
	state := validTestState(t)
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "projectFingerprints")
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil || !strings.Contains(err.Error(), "project fingerprints") {
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
		ProjectFingerprints: map[string]string{
			"project": "project-content",
		},
		ProposalDigest: digest,
		ProjectMappings: []ProjectMapping{{
			ProjectID: "project", PortablePath: "project", PhysicalRoot: projectRoot,
		}},
		Proposal: proposal,
	}
}
