package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloister.io/internal/config"
	"cloister.io/internal/workspace/scan"
	"github.com/spf13/cobra"
)

func TestWorkspaceCommandContract(t *testing.T) {
	if workspaceCmd.Use != "workspace" {
		t.Fatalf("Use = %q", workspaceCmd.Use)
	}
	for _, name := range []string{"scan", "review", "show", "apply"} {
		if command, _, err := workspaceCmd.Find([]string{name}); err != nil || command.Name() != name {
			t.Fatalf("workspace %s is not registered: %v", name, err)
		}
	}
}

func TestWorkspaceStatePathRejectsUnsafeProfile(t *testing.T) {
	for _, profile := range []string{"", ".", "../other", "a/b", `a\b`} {
		if _, err := workspaceStatePath(t.TempDir(), profile); err == nil {
			t.Fatalf("workspaceStatePath accepted %q", profile)
		}
	}
	path, err := workspaceStatePath("/home/test", "work_1")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/home/test/.cloister/state/workspace-scans/work_1.json" {
		t.Fatalf("path = %q", path)
	}
}

func TestReviewRequiresEveryDecisionAndFinalConfirmation(t *testing.T) {
	proposal := scan.Proposal{
		Findings: []scan.Finding{
			{ProjectID: "app", Path: ".env", Class: scan.ClassSecretLocalConfig, Reason: "local config", Recommendation: scan.RecommendationReview, Decision: scan.DecisionReview},
			{ProjectID: "app", Path: "large.bin", Class: scan.ClassUnknownLarge, Reason: "large file", Recommendation: scan.RecommendationReview, Decision: scan.DecisionReview},
		},
		Exclusions: []scan.Exclusion{{ProjectID: "app", Path: ".env", Class: scan.ClassSecretLocalConfig, Reason: "local config"}},
	}
	var output bytes.Buffer
	if err := reviewProposal(&proposal, strings.NewReader("i\ne\ny\n"), &output); err != nil {
		t.Fatal(err)
	}
	if proposal.Findings[0].Decision != scan.DecisionInclude || proposal.Findings[1].Decision != scan.DecisionExclude {
		t.Fatalf("decisions = %#v", proposal.Findings)
	}
	wantExclusions := []scan.Exclusion{{ProjectID: "app", Path: "large.bin", Class: scan.ClassUnknownLarge, Reason: "large file"}}
	if !reflect.DeepEqual(proposal.Exclusions, wantExclusions) {
		t.Fatalf("review exclusions = %#v, want %#v", proposal.Exclusions, wantExclusions)
	}
	if !strings.Contains(output.String(), "Env and secrets") || !strings.Contains(output.String(), "Unknown large files") {
		t.Fatalf("sectioned output missing: %s", output.String())
	}

	proposal.Findings[0].Decision = scan.DecisionReview
	proposal.Findings[1].Decision = scan.DecisionReview
	if err := reviewProposal(&proposal, strings.NewReader("i\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("EOF was accepted")
	}
}

func TestReviewResolvesRepositoryCandidateDecisions(t *testing.T) {
	proposal := scan.Proposal{
		Projects: []scan.Project{{
			ID: "outer", Path: "outer", Kind: scan.ProjectRepository,
			NestedRepositories: 1, Reason: "contains 1 nested repository; synchronizing it would overlap it",
			Recommendation: scan.RecommendationReview, Decision: scan.DecisionReview,
		}},
		Findings:   []scan.Finding{},
		Exclusions: []scan.Exclusion{},
	}
	var output bytes.Buffer
	if err := reviewProposal(&proposal, strings.NewReader("e\ny\n"), &output); err != nil {
		t.Fatal(err)
	}
	if proposal.Projects[0].Decision != scan.DecisionExclude {
		t.Fatalf("project decision = %q", proposal.Projects[0].Decision)
	}
	if !strings.Contains(output.String(), "Repository projects") ||
		!strings.Contains(output.String(), "contains 1 nested repository") {
		t.Fatalf("project review context missing: %s", output.String())
	}
}

func TestReviewBulkIncludeAppliesToRemainingAgentConfigFindings(t *testing.T) {
	proposal := reviewTestProposal(
		reviewFinding("app", "AGENTS.md", scan.ClassAgentConfig),
		reviewFinding("app", ".cursor/rules/style.mdc", scan.ClassAgentConfig),
		reviewFinding("tool", "CLAUDE.md", scan.ClassAgentConfig),
	)
	var output bytes.Buffer
	if err := reviewProposal(&proposal, strings.NewReader("ia\ny\n"), &output); err != nil {
		t.Fatal(err)
	}
	for _, finding := range proposal.Findings {
		if finding.Decision != scan.DecisionInclude {
			t.Fatalf("finding = %#v, want include", finding)
		}
	}
	if !strings.Contains(output.String(), "ia=include-all") || !strings.Contains(output.String(), "current section") {
		t.Fatalf("bulk options missing from prompt: %s", output.String())
	}
}

func TestReviewBulkExcludeAppliesToRemainingSecretFindings(t *testing.T) {
	proposal := reviewTestProposal(
		reviewFinding("app", ".env", scan.ClassSecretLocalConfig),
		reviewFinding("app", ".env.local", scan.ClassSecretLocalConfig),
	)
	if err := reviewProposal(&proposal, strings.NewReader("exclude-all\ny\n"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, finding := range proposal.Findings {
		if finding.Decision != scan.DecisionExclude {
			t.Fatalf("finding = %#v, want exclude", finding)
		}
	}
	if len(proposal.Exclusions) != 2 {
		t.Fatalf("exclusions = %#v, want both secret findings", proposal.Exclusions)
	}
}

func TestReviewBulkDecisionDoesNotCrossSectionBoundary(t *testing.T) {
	proposal := reviewTestProposal(
		reviewFinding("app", "AGENTS.md", scan.ClassAgentConfig),
		reviewFinding("tool", "CLAUDE.md", scan.ClassAgentConfig),
		reviewFinding("app", "large.bin", scan.ClassUnknownLarge),
	)
	if err := reviewProposal(&proposal, strings.NewReader("include-all\ne\ny\n"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if proposal.Findings[0].Decision != scan.DecisionInclude ||
		proposal.Findings[1].Decision != scan.DecisionInclude ||
		proposal.Findings[2].Decision != scan.DecisionExclude {
		t.Fatalf("bulk decision crossed section: %#v", proposal.Findings)
	}
}

func TestReviewInvalidInputAndEOFFailClosedWithoutChangingProposal(t *testing.T) {
	for name, input := range map[string]string{
		"invalid after decision": "i\nall\n",
		"EOF after decision":     "i\n",
	} {
		t.Run(name, func(t *testing.T) {
			proposal := reviewTestProposal(
				reviewFinding("app", ".env", scan.ClassSecretLocalConfig),
				reviewFinding("app", ".env.local", scan.ClassSecretLocalConfig),
			)
			before := proposal
			before.Findings = append([]scan.Finding(nil), proposal.Findings...)
			before.Exclusions = append([]scan.Exclusion{}, proposal.Exclusions...)
			if err := reviewProposal(&proposal, strings.NewReader(input), &bytes.Buffer{}); err == nil {
				t.Fatalf("%s input was accepted", name)
			}
			if !reflect.DeepEqual(proposal, before) {
				t.Fatalf("proposal changed after %s: got %#v, want %#v", name, proposal, before)
			}
		})
	}
}

func TestReviewCancellationAfterBulkDecisionPreservesProposal(t *testing.T) {
	proposal := reviewTestProposal(
		reviewFinding("app", ".env", scan.ClassSecretLocalConfig),
		reviewFinding("app", ".env.local", scan.ClassSecretLocalConfig),
	)
	before := proposal
	before.Findings = append([]scan.Finding(nil), proposal.Findings...)
	before.Exclusions = append([]scan.Exclusion{}, proposal.Exclusions...)
	if err := reviewProposal(&proposal, strings.NewReader("ea\nn\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("review cancellation was accepted")
	}
	if !reflect.DeepEqual(proposal, before) {
		t.Fatalf("proposal changed after cancellation: got %#v, want %#v", proposal, before)
	}
}

func reviewTestProposal(findings ...scan.Finding) scan.Proposal {
	return scan.Proposal{Findings: findings, Exclusions: []scan.Exclusion{}}
}

func reviewFinding(projectID, path string, class scan.FindingClass) scan.Finding {
	return scan.Finding{
		ProjectID: projectID, Path: path, Class: class, Reason: "review required",
		Recommendation: scan.RecommendationReview, Decision: scan.DecisionReview,
	}
}

func TestBuildAppliedWorkspaceUsesExactExclusions(t *testing.T) {
	proposal := scan.Proposal{
		Projects: []scan.Project{{
			ID: "app", Path: "apps/app", Kind: scan.ProjectShared, Reason: "selected project",
			Recommendation: scan.RecommendationInclude, Decision: scan.DecisionInclude,
		}},
		Findings: []scan.Finding{
			{ProjectID: "app", Path: "tmp", Directory: true, Decision: scan.DecisionExclude},
			{ProjectID: "app", Path: "keep.txt", Decision: scan.DecisionInclude},
		},
		Policy: scan.Policy{
			Selectors:            []string{"apps/app"},
			Ignore:               []string{"baseline/"},
			ProjectIgnore:        map[string][]string{"apps/app": {"existing/"}},
			MaxStagingFileSize:   "256 MiB",
			MaxEntriesPerProject: 42,
		},
	}
	got, err := buildAppliedWorkspace("/workspace", proposal)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != config.WorkspaceModeWorkspace || got.Root != "/workspace" || got.MaxEntryCount != 42 {
		t.Fatalf("workspace = %#v", got)
	}
	ignores := strings.Join(got.ProjectIgnore["apps/app"], ",")
	if ignores != "existing/,tmp/" || strings.Contains(ignores, "keep.txt") {
		t.Fatalf("project ignores = %q", ignores)
	}
}

func TestBuildAppliedWorkspaceRejectsOverlappingSelectedProjects(t *testing.T) {
	proposal := scan.Proposal{
		Projects: []scan.Project{
			{
				ID: "outer", Path: "outer", Kind: scan.ProjectRepository, Reason: "canonical repository",
				Recommendation: scan.RecommendationInclude, Decision: scan.DecisionInclude,
			},
			{
				ID: "inner", Path: "outer/inner", Kind: scan.ProjectRepository, Reason: "canonical repository",
				Recommendation: scan.RecommendationInclude, Decision: scan.DecisionInclude,
			},
		},
		Findings: []scan.Finding{},
		Policy: scan.Policy{
			Ignore: []string{}, ProjectIgnore: map[string][]string{},
		},
	}
	_, err := buildAppliedWorkspace("/workspace", proposal)
	const want = `selected projects "outer" and "outer/inner" overlap; keep exactly one of them`
	if err == nil || err.Error() != want {
		t.Fatalf("overlap error = %v, want %q", err, want)
	}

	proposal.Projects[0].Decision = scan.DecisionExclude
	applied, err := buildAppliedWorkspace("/workspace", proposal)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied.Selectors, []string{"outer/inner"}) {
		t.Fatalf("selectors = %#v", applied.Selectors)
	}

	proposal.Projects[1].Decision = scan.DecisionExclude
	if _, err := buildAppliedWorkspace("/workspace", proposal); err == nil ||
		!strings.Contains(err.Error(), "selects no projects") {
		t.Fatalf("empty project selection error = %v", err)
	}
}

func TestWorkspaceApplyCancellationLeavesConfigUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{Profiles: map[string]*config.Profile{"work": {Color: "blue"}}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveAppliedConfig(path, cfg, "work", config.WorkspaceConfig{Mode: config.WorkspaceModeWorkspace}, strings.NewReader("n\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("cancel was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("config changed after cancellation")
	}
}

func TestSelectWorkspaceSourceUsesConfiguredRootForProfileAndNestedPath(t *testing.T) {
	home := t.TempDir()
	start := filepath.Join(home, "code")
	root := filepath.Join(home, "workspace")
	project := filepath.Join(root, "apps", "api")
	for _, dir := range []string{start, project} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Profiles: map[string]*config.Profile{
		"work": {StartDir: start, Workspace: config.WorkspaceConfig{Root: root, Selectors: []string{"apps/*"}}},
	}}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"work", project} {
		selected, err := selectWorkspaceSource(target, home, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if selected.sourceRoot != canonicalRoot {
			t.Fatalf("target %q source root = %q, want %q", target, selected.sourceRoot, canonicalRoot)
		}
	}
}

func TestValidateProjectMappingsRejectsExternalCollision(t *testing.T) {
	root := t.TempDir()
	expected := filepath.Join(root, "apps", "api")
	external := filepath.Join(t.TempDir(), "api")
	for _, dir := range []string{expected, external} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	proposal := scan.Proposal{Projects: []scan.Project{{ID: "api", Path: "apps/api", Kind: scan.ProjectShared}}}
	mappings := []scan.ProjectMapping{{ProjectID: "api", PortablePath: "apps/api", PhysicalRoot: external}}
	if err := validatePortableProjectMappings(root, proposal, mappings); err == nil {
		t.Fatal("external mapping collision was accepted")
	}
}

func TestHumanProposalOutputSummarizesIncludedSource(t *testing.T) {
	proposal := scan.Proposal{}
	for index := 0; index < 10_000; index++ {
		proposal.Findings = append(proposal.Findings, scan.Finding{
			ProjectID: "app", Path: fmt.Sprintf("src/file-%05d.go", index),
			Class: scan.ClassSource, Decision: scan.DecisionInclude,
		})
	}
	var output bytes.Buffer
	printProposalSections(&output, proposal, false)
	if output.Len() > 2_000 {
		t.Fatalf("human output too large: %d bytes", output.Len())
	}
	if !strings.Contains(output.String(), "10000") {
		t.Fatalf("source count missing: %s", output.String())
	}
}

func TestHumanProposalOutputSummarizesIncludedDatabaseScripts(t *testing.T) {
	proposal := scan.Proposal{
		Findings: []scan.Finding{
			{ProjectID: "app", Path: ".env", Class: scan.ClassSecretLocalConfig, Decision: scan.DecisionReview},
			{ProjectID: "app", Path: "backup.sql", Class: scan.ClassDatabaseDump, Decision: scan.DecisionExclude},
			{ProjectID: "app", Path: "package.json", Class: scan.ClassApplicationManifest, Decision: scan.DecisionInclude},
		},
		Commands: []scan.Command{{ProjectID: "app", Path: "package.json", Name: "test"}},
		Services: []scan.Service{{ProjectID: "app", Path: "compose.yaml", Name: "db"}},
	}
	for index := 0; index < 10_000; index++ {
		proposal.Findings = append(proposal.Findings, scan.Finding{
			ProjectID: "app", Path: fmt.Sprintf("sql/query-%05d.sql", index),
			Class: scan.ClassDatabaseScript, Decision: scan.DecisionInclude,
		})
	}
	var output bytes.Buffer
	printProposalSections(&output, proposal, false)
	text := output.String()
	if output.Len() > 2_000 {
		t.Fatalf("human output too large: %d bytes", output.Len())
	}
	if !strings.Contains(text, "Local databases, dumps, and scripts (10001)") {
		t.Fatalf("database section count missing: %s", text)
	}
	if strings.Contains(text, "sql/query-00000.sql") || strings.Contains(text, "sql/query-09999.sql") {
		t.Fatalf("included database script was listed: %s", text)
	}
	if !strings.Contains(text, ".env") || !strings.Contains(text, "backup.sql") {
		t.Fatalf("review and exclude findings missing: %s", text)
	}
	if strings.Contains(text, "application_manifest") {
		t.Fatalf("included manifest finding was listed: %s", text)
	}
	if !strings.Contains(text, "command test") || !strings.Contains(text, "service db") {
		t.Fatalf("command and service inventory missing: %s", text)
	}
}

func TestLoadWorkspaceSourceDoesNotFallbackFromMalformedManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "manifest"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest", "projects.json"), []byte(`{"projects":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkspaceSource(root, t.TempDir(), config.WorkspaceConfig{}); err == nil {
		t.Fatal("malformed present manifest fell back to generic")
	}
}

func TestLoadWorkspaceSourceDiscoversRepositoriesOutsideConfiguredSelectors(t *testing.T) {
	root := t.TempDir()
	for _, repository := range []string{"apps/existing", "groups/deep/new"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(repository), ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "apps", "scratch"), 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := loadWorkspaceSource(root, t.TempDir(), config.WorkspaceConfig{
		Selectors: []string{"apps/existing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Adapter != scan.SourceAdapterRepository {
		t.Fatalf("adapter = %q", result.Adapter)
	}
	var paths []string
	for _, project := range result.Projects {
		paths = append(paths, project.Path)
	}
	if !reflect.DeepEqual(paths, []string{"apps/existing", "groups/deep/new"}) {
		t.Fatalf("repository paths = %#v", paths)
	}
}

func TestSaveAppliedConfigPreservesUnrelatedConfigurationAndPrintsFieldDelta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		MemoryBudget: 24,
		Profiles: map[string]*config.Profile{
			"work":  {Color: "blue", Stacks: []string{"go"}},
			"other": {Color: "green", Memory: 8},
		},
	}
	next := config.WorkspaceConfig{
		Mode: config.WorkspaceModeWorkspace, Root: "/workspace",
		Selectors: []string{"apps/api"}, Ignore: []string{"tmp/"},
		ProjectIgnore: map[string][]string{"apps/api": {"local.db"}},
		MaxEntryCount: 123, MaxStagingFileSize: "256 MiB",
	}
	var output bytes.Buffer
	if err := saveAppliedConfig(path, cfg, "work", next, strings.NewReader("y\n"), &output); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MemoryBudget != 24 || loaded.Profiles["other"].Color != "green" || loaded.Profiles["other"].Memory != 8 {
		t.Fatalf("unrelated config changed: %#v", loaded)
	}
	if loaded.Profiles["work"].Color != "blue" || !reflect.DeepEqual(loaded.Profiles["work"].Workspace, next) {
		t.Fatalf("target profile mismatch: %#v", loaded.Profiles["work"])
	}
	text := output.String()
	if strings.Contains(text, "color") || !strings.Contains(text, "selectors (pinned approved projects)") {
		t.Fatalf("delta is not focused: %s", text)
	}
}

func TestLoadFreshRejectsTamperedExternalMappingToCoincidentalLocalPath(t *testing.T) {
	home, root, cfg := setupExternalMappingWorkspace(t)
	statePath := scanThenLoadStatePath(t, home, root, cfg, "dev")
	state, err := scan.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ProjectMappings) != 1 || state.ProjectMappings[0].PhysicalRoot == "" {
		t.Fatalf("expected persisted external mapping: %#v", state.ProjectMappings)
	}
	localRoot := mustCanonicalDir(t, filepath.Join(root, filepath.FromSlash(state.ProjectMappings[0].PortablePath)))
	if localRoot == state.ProjectMappings[0].PhysicalRoot {
		t.Fatal("fixture did not produce an external physical root")
	}

	state.ProjectMappings[0].PhysicalRoot = localRoot
	if err := scan.SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}

	if _, _, err := loadFreshWorkspaceState(home, "dev", cfg); err == nil {
		t.Fatal("loadFresh accepted a tampered coincidental local mapping")
	}
	if err := runWorkspaceReview(newIOCommand(strings.NewReader("i\ny\n"), &bytes.Buffer{}), "dev"); err == nil {
		t.Fatal("review accepted a tampered coincidental local mapping")
	}
	if err := runWorkspaceApply(newIOCommand(strings.NewReader("y\n"), &bytes.Buffer{}), "dev"); err == nil {
		t.Fatal("apply accepted a tampered coincidental local mapping")
	}
}

func TestLoadFreshRejectsStaleConfigFingerprint(t *testing.T) {
	home, _, cfg := setupGenericScanWorkspace(t, "package main\n")
	scanThenLoadStatePath(t, home, cfg.Profiles["dev"].StartDir, cfg, "dev")
	cfg.Profiles["dev"].Workspace.Ignore = []string{"tmp/"}
	if _, _, err := loadFreshWorkspaceState(home, "dev", cfg); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale config error = %v", err)
	}
}

func TestLoadFreshRejectsStaleSourceCatalogFingerprint(t *testing.T) {
	home, root, cfg := setupManifestScanWorkspace(t)
	scanThenLoadStatePath(t, home, root, cfg, "dev")
	manifest := filepath.Join(root, "manifest", "projects.json")
	if err := os.WriteFile(manifest, []byte(`{"formatVersion":1,"projects":[{"name":"api","path":"apps/api"}],"policy":{"ignore":["cache/"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadFreshWorkspaceState(home, "dev", cfg); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale source error = %v", err)
	}
}

func TestLoadFreshRejectsProjectTreeDriftBeforeReviewAndApply(t *testing.T) {
	mutations := map[string]func(t *testing.T, path string){
		"added": func(t *testing.T, path string) {
			writeWorkspaceFile(t, filepath.Join(filepath.Dir(path), "added.go"), "package added\n")
		},
		"removed": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		},
		"renamed": func(t *testing.T, path string) {
			if err := os.Rename(path, filepath.Join(filepath.Dir(path), "renamed.go")); err != nil {
				t.Fatal(err)
			}
		},
		"size only": func(t *testing.T, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			writeWorkspaceFile(t, path, "package main\n\nvar changed = true\n")
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
		"mtime only": func(t *testing.T, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := info.ModTime().Add(2 * time.Second)
			if err := os.Chtimes(path, changed, changed); err != nil {
				t.Fatal(err)
			}
		},
		"new secret path": func(t *testing.T, path string) {
			writeWorkspaceFile(t, filepath.Join(filepath.Dir(path), ".ssh", "credentials"), "local-only\n")
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			home, root, cfg := setupGenericScanWorkspace(t, "package main\n")
			statePath := scanThenLoadStatePath(t, home, root, cfg, "dev")
			configPath := filepath.Join(home, ".cloister", "config.yaml")
			stateBefore, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			configBefore, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			mutate(t, filepath.Join(root, "apps", "api", "main.go"))

			if _, _, err := loadFreshWorkspaceState(home, "dev", cfg); err == nil || !strings.Contains(err.Error(), "project tree changed") {
				t.Fatalf("loadFreshWorkspaceState() error = %v", err)
			}
			if err := runWorkspaceReview(newIOCommand(strings.NewReader("y\n"), &bytes.Buffer{}), "dev"); err == nil || !strings.Contains(err.Error(), "project tree changed") {
				t.Fatalf("runWorkspaceReview() error = %v", err)
			}
			if err := runWorkspaceApply(newIOCommand(strings.NewReader("y\n"), &bytes.Buffer{}), "dev"); err == nil || !strings.Contains(err.Error(), "project tree changed") {
				t.Fatalf("runWorkspaceApply() error = %v", err)
			}
			stateAfter, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			configAfter, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stateBefore, stateAfter) || !bytes.Equal(configBefore, configAfter) {
				t.Fatal("stale review or apply wrote state or configuration")
			}
		})
	}
}

func TestLoadFreshRejectsProposalDigestMismatch(t *testing.T) {
	home, _, cfg := setupGenericScanWorkspace(t, "package main\n")
	statePath := scanThenLoadStatePath(t, home, cfg.Profiles["dev"].StartDir, cfg, "dev")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["proposalDigest"] = strings.Repeat("0", 64)
	tampered, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, append(tampered, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadFreshWorkspaceState(home, "dev", cfg); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("digest mismatch error = %v", err)
	}
}

func TestReviewCancelLeavesPersistedStateUnchanged(t *testing.T) {
	home, _, cfg := setupGenericScanWorkspace(t, "package main\n")
	writeWorkspaceFile(t, filepath.Join(cfg.Profiles["dev"].StartDir, "apps", "api", ".env"), "TOKEN=local-only\n")
	statePath := scanThenLoadStatePath(t, home, cfg.Profiles["dev"].StartDir, cfg, "dev")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWorkspaceReview(newIOCommand(strings.NewReader("e\nn\n"), &bytes.Buffer{}), "dev"); err == nil {
		t.Fatal("review cancel was accepted")
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("review cancel rewrote persisted state")
	}
}

func TestWorkspaceScanJSONOmitsLocalRootsAndSentinelContents(t *testing.T) {
	const sentinel = "sentinel-local-secret-value"
	home, _, cfg := setupGenericScanWorkspace(t, "package main\n")
	root := cfg.Profiles["dev"].StartDir
	writeWorkspaceFile(t, filepath.Join(root, "apps", "api", ".env"), sentinel+"\n")
	previous := workspaceScanJSON
	workspaceScanJSON = true
	t.Cleanup(func() { workspaceScanJSON = previous })

	var output bytes.Buffer
	if err := runWorkspaceScan(newIOCommand(nil, &output), "dev"); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	canonicalRoot := mustCanonicalDir(t, root)
	for _, leak := range []string{sentinel, canonicalRoot, root, home} {
		if leak != "" && strings.Contains(text, leak) {
			t.Fatalf("JSON leaked %q: %s", leak, text)
		}
	}
	var proposal scan.Proposal
	if err := json.Unmarshal(output.Bytes(), &proposal); err != nil {
		t.Fatalf("JSON was not a portable proposal: %v\n%s", err, text)
	}
	if proposal.Source.Root != "." {
		t.Fatalf("source root = %q", proposal.Source.Root)
	}
}

func TestWorkspaceCommandFileDoesNotImportLifecyclePackages(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "workspace.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"cloister.io/internal/vm",
		"cloister.io/internal/vm/colima",
		"cloister.io/internal/vm/lume",
		"cloister.io/internal/lifecycle",
		"cloister.io/internal/broker",
		"cloister.io/internal/vcsbroker",
	}
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		for _, prefix := range forbidden {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				t.Fatalf("workspace command imported %q", path)
			}
		}
	}
}

func setupGenericScanWorkspace(t *testing.T, sourceContents string) (string, string, *config.Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "workspace")
	writeWorkspaceFile(t, filepath.Join(root, "apps", "api", "main.go"), sourceContents)
	writeWorkspaceFile(t, filepath.Join(root, "apps", "api", ".git", "HEAD"), "ref: refs/heads/main\n")
	cfg := &config.Config{Profiles: map[string]*config.Profile{
		"dev": {StartDir: root, Workspace: config.WorkspaceConfig{Selectors: []string{"apps/*"}}},
	}}
	if err := config.Save(filepath.Join(home, ".cloister", "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(filepath.Join(home, ".cloister", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return home, root, loaded
}

func setupManifestScanWorkspace(t *testing.T) (string, string, *config.Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "workspace")
	writeWorkspaceFile(t, filepath.Join(root, "projects", "apps", "api", "main.go"), "package main\n")
	writeWorkspaceFile(t, filepath.Join(root, "manifest", "projects.json"), `{"formatVersion":1,"projects":[{"name":"api","path":"apps/api"}]}`)
	cfg := &config.Config{Profiles: map[string]*config.Profile{
		"dev": {StartDir: root},
	}}
	if err := config.Save(filepath.Join(home, ".cloister", "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(filepath.Join(home, ".cloister", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return home, root, loaded
}

func setupExternalMappingWorkspace(t *testing.T) (string, string, *config.Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "workspace")
	external := t.TempDir()
	writeWorkspaceFile(t, filepath.Join(root, "manifest", "projects.json"), `{"formatVersion":1,"projects":[{"name":"api","path":"apps/api"}]}`)
	writeWorkspaceFile(t, filepath.Join(root, "projects", "apps", "api", "main.go"), "package local\n")
	writeWorkspaceFile(t, filepath.Join(external, "apps", "api", "main.go"), "package external\n")
	t.Setenv("WORKSPACE_PROJECTS_DIR", mustCanonicalDir(t, external))
	cfg := &config.Config{Profiles: map[string]*config.Profile{
		"dev": {StartDir: root},
	}}
	if err := config.Save(filepath.Join(home, ".cloister", "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(filepath.Join(home, ".cloister", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return home, root, loaded
}

func scanThenLoadStatePath(t *testing.T, home, root string, cfg *config.Config, profile string) string {
	t.Helper()
	previous := workspaceScanJSON
	workspaceScanJSON = false
	t.Cleanup(func() { workspaceScanJSON = previous })
	if err := runWorkspaceScan(newIOCommand(nil, &bytes.Buffer{}), profile); err != nil {
		t.Fatal(err)
	}
	path, err := workspaceStatePath(home, profile)
	if err != nil {
		t.Fatal(err)
	}
	_ = root
	_ = cfg
	return path
}

func newIOCommand(input *strings.Reader, output *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{}
	if input != nil {
		cmd.SetIn(input)
	}
	cmd.SetOut(output)
	cmd.SetErr(output)
	return cmd
}

func writeWorkspaceFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustCanonicalDir(t *testing.T, path string) string {
	t.Helper()
	canonical, err := canonicalWorkspaceRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
