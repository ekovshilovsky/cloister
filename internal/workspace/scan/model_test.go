package scan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProposalJSONIncludesPortableCloudFieldsAndStableCollections(t *testing.T) {
	proposal := Proposal{
		SchemaVersion: CurrentSchemaVersion,
		CreatedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Generator:     "test",
		Source:        SourceMetadata{Root: ".", Adapter: SourceAdapterGeneric},
		Projects: []Project{
			{ID: "z", Path: "tools/z", Kind: ProjectLocal},
			{ID: "a", Path: "apps/a", Kind: ProjectShared},
		},
		Findings: []Finding{
			{Class: ClassSource, ProjectID: "z", Path: "z.go", Size: 1, Reason: "source file", Recommendation: RecommendationInclude, Decision: DecisionInclude},
			{Class: ClassSource, ProjectID: "a", Path: "a.go", Size: 1, Reason: "source file", Recommendation: RecommendationInclude, Decision: DecisionInclude},
		},
		Runtimes: []Runtime{},
		Commands: []Command{},
		Services: []Service{},
		Policy: Policy{
			Selectors:            []string{"tools/*", "apps/*"},
			Ignore:               []string{"tmp", ".DS_Store"},
			ProjectIgnore:        map[string][]string{"tools/z": {"cache", "output"}},
			MaxEntriesPerProject: 10,
			MaxBytesPerProject:   100,
			PrunePatterns:        []string{"node_modules", ".git"},
		},
		Exclusions:               []Exclusion{},
		CloudReadiness:           CloudReadinessLocalOnly,
		UnansweredCloudQuestions: []string{},
	}

	first, err := MarshalProposal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalProposal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("serialization is not deterministic:\n%s\n%s", first, second)
	}
	if !strings.Contains(string(first), `"cloudReadiness": "local_only"`) ||
		!strings.Contains(string(first), `"unansweredCloudQuestions": []`) ||
		!strings.Contains(string(first), `"adapter": "generic"`) ||
		!strings.Contains(string(first), `"projectIgnore": {`) {
		t.Fatalf("required portable fields missing from JSON: %s", first)
	}

	var decoded Proposal
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Projects[0].ID != "a" || decoded.Findings[0].ProjectID != "a" {
		t.Fatalf("collections were not sorted: %#v", decoded)
	}
	if got := decoded.Policy.PrunePatterns; len(got) != 2 || got[0] != ".git" {
		t.Fatalf("prune patterns not sorted: %v", got)
	}
	if got := decoded.Policy.Selectors; len(got) != 2 || got[0] != "apps/*" {
		t.Fatalf("selectors not sorted: %v", got)
	}
	if got := decoded.Policy.ProjectIgnore["tools/z"]; len(got) != 2 || got[0] != "cache" {
		t.Fatalf("project ignore not sorted: %v", got)
	}
}

func TestValidateProposalRejectsIncompleteAndUnknownSchema(t *testing.T) {
	valid := validTestProposal()
	valid.SchemaVersion = CurrentSchemaVersion + 1
	if err := ValidateProposal(valid); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("unknown schema error = %v", err)
	}

	valid = validTestProposal()
	valid.Projects[0].Path = "/private/project"
	if err := ValidateProposal(valid); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("absolute project path error = %v", err)
	}

	valid = validTestProposal()
	valid.UnansweredCloudQuestions = nil
	if err := ValidateProposal(valid); err == nil || !strings.Contains(err.Error(), "unansweredCloudQuestions") {
		t.Fatalf("missing cloud questions error = %v", err)
	}

	valid = validTestProposal()
	valid.Commands = []Command{{ProjectID: "project", Name: "test", Path: "/package.json"}}
	if err := ValidateProposal(valid); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("absolute command path error = %v", err)
	}

	valid = validTestProposal()
	valid.Policy.ProjectIgnore = nil
	if err := ValidateProposal(valid); err == nil || !strings.Contains(err.Error(), "collection fields") {
		t.Fatalf("missing project ignore error = %v", err)
	}

	valid = validTestProposal()
	valid.Policy.ProjectIgnore = map[string][]string{"missing": {"cache"}}
	if err := ValidateProposal(valid); err == nil || !strings.Contains(err.Error(), "unknown project") {
		t.Fatalf("unknown project ignore identity error = %v", err)
	}

	valid = validTestProposal()
	valid.Projects[0] = Project{ID: "stable-id", Path: "apps/project", Kind: ProjectShared}
	valid.Policy.ProjectIgnore = map[string][]string{"apps/project": {"cache"}}
	if err := ValidateProposal(valid); err != nil {
		t.Fatalf("projectIgnore keyed by root-relative project path was rejected: %v", err)
	}

	valid = validTestProposal()
	valid.Source.Adapter = ""
	if err := ValidateProposal(valid); err == nil || !strings.Contains(err.Error(), "adapter") {
		t.Fatalf("empty adapter error = %v", err)
	}

	valid = validTestProposal()
	valid.Source.Adapter = "unknown"
	if err := ValidateProposal(valid); err == nil || !strings.Contains(err.Error(), "adapter") {
		t.Fatalf("unknown adapter error = %v", err)
	}

	valid = validTestProposal()
	valid.Projects = []Project{
		{ID: "one", Path: "apps/api", Kind: ProjectShared},
		{ID: "two", Path: "apps/api", Kind: ProjectLocal},
	}
	if err := ValidateProposal(valid); err == nil || !strings.Contains(err.Error(), "duplicate project path") {
		t.Fatalf("duplicate project path error = %v", err)
	}
}

func TestMarshalProposalDoesNotMutateCallerCollections(t *testing.T) {
	proposal := validTestProposal()
	proposal.Policy.Selectors = []string{"z", "a"}
	proposal.Policy.Ignore = []string{"z", "a"}
	proposal.Policy.PrunePatterns = []string{"z", "a"}
	proposal.Policy.ProjectIgnore = map[string][]string{"project": {"z", "a"}}

	if _, err := MarshalProposal(proposal); err != nil {
		t.Fatal(err)
	}
	if proposal.Policy.Selectors[0] != "z" || proposal.Policy.Ignore[0] != "z" ||
		proposal.Policy.PrunePatterns[0] != "z" || proposal.Policy.ProjectIgnore["project"][0] != "z" {
		t.Fatalf("MarshalProposal mutated caller collections: %#v", proposal.Policy)
	}
}

func TestRebuildExclusionsExactlyTracksFinalDecisions(t *testing.T) {
	proposal := validTestProposal()
	proposal.Findings = []Finding{
		{Class: ClassSecretLocalConfig, ProjectID: "project", Path: "z.env", Size: 1, Reason: "secret", Recommendation: RecommendationReview, Decision: DecisionExclude},
		{Class: ClassSource, ProjectID: "project", Path: "keep.go", Size: 1, Reason: "source", Recommendation: RecommendationInclude, Decision: DecisionInclude},
		{Class: ClassDatabase, ProjectID: "project", Path: "a.db", Size: 1, Reason: "database", Recommendation: RecommendationExclude, Decision: DecisionExclude},
	}
	proposal.Exclusions = []Exclusion{{ProjectID: "project", Path: "stale", Class: ClassSource, Reason: "stale"}}

	RebuildExclusions(&proposal)

	want := []Exclusion{
		{ProjectID: "project", Path: "a.db", Class: ClassDatabase, Reason: "database"},
		{ProjectID: "project", Path: "z.env", Class: ClassSecretLocalConfig, Reason: "secret"},
	}
	if !reflect.DeepEqual(proposal.Exclusions, want) {
		t.Fatalf("exclusions = %#v, want %#v", proposal.Exclusions, want)
	}
}

func TestValidateProposalRejectsExclusionsThatDoNotExactlyMatchDecisions(t *testing.T) {
	excludedFinding := Finding{
		Class: ClassDatabase, ProjectID: "project", Path: "local.db", Size: 1,
		Reason: "database", Recommendation: RecommendationExclude, Decision: DecisionExclude,
	}
	cases := map[string][]Exclusion{
		"missing":      {},
		"extra":        {{ProjectID: "project", Path: "extra.db", Class: ClassDatabase, Reason: "database"}},
		"duplicate":    {{ProjectID: "project", Path: "local.db", Class: ClassDatabase, Reason: "database"}, {ProjectID: "project", Path: "local.db", Class: ClassDatabase, Reason: "database"}},
		"wrong class":  {{ProjectID: "project", Path: "local.db", Class: ClassSource, Reason: "database"}},
		"wrong reason": {{ProjectID: "project", Path: "local.db", Class: ClassDatabase, Reason: "different"}},
	}
	for name, exclusions := range cases {
		t.Run(name, func(t *testing.T) {
			proposal := validTestProposal()
			proposal.Findings = []Finding{excludedFinding}
			proposal.Exclusions = exclusions
			if err := ValidateProposal(proposal); err == nil || !strings.Contains(err.Error(), "exclusions") {
				t.Fatalf("ValidateProposal() error = %v", err)
			}
		})
	}
}

func validTestProposal() Proposal {
	return Proposal{
		SchemaVersion: CurrentSchemaVersion,
		CreatedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Generator:     "test",
		Source:        SourceMetadata{Root: ".", Adapter: SourceAdapterGeneric},
		Projects:      []Project{{ID: "project", Path: "project", Kind: ProjectShared}},
		Findings:      []Finding{},
		Runtimes:      []Runtime{},
		Commands:      []Command{},
		Services:      []Service{},
		Policy: Policy{
			Selectors:            []string{},
			Ignore:               []string{},
			ProjectIgnore:        map[string][]string{},
			MaxEntriesPerProject: 10,
			MaxBytesPerProject:   100,
			PrunePatterns:        []string{},
		},
		Exclusions:               []Exclusion{},
		CloudReadiness:           CloudReadinessLocalOnly,
		UnansweredCloudQuestions: []string{},
	}
}
