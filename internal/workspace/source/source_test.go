package source

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cloister.io/internal/config"
	"cloister.io/internal/workspace/scan"
)

func TestGenericSelectorUsesWorkspaceSelectionSemantics(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "apps/api", "services/web")

	result, err := (GenericSelector{
		StartDir: root,
		Home:     t.TempDir(),
		Config: config.WorkspaceConfig{
			Selectors:     []string{"apps/*", "services/*"},
			Ignore:        []string{"tmp/"},
			ProjectIgnore: map[string][]string{"apps/api": {"generated/"}},
			MaxEntryCount: 42,
		},
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []scan.ProjectDescriptor{
		{ID: "apps/api", Path: "apps/api", Kind: scan.ProjectShared},
		{ID: "services/web", Path: "services/web", Kind: scan.ProjectShared},
	}
	if !reflect.DeepEqual(result.Projects, want) {
		t.Fatalf("projects = %#v, want %#v", result.Projects, want)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Root != canonicalRoot || result.Adapter != scan.SourceAdapterGeneric {
		t.Fatalf("source metadata = %q/%q", result.Root, result.Adapter)
	}
	if !reflect.DeepEqual(result.Policy.Selectors, []string{"apps/*", "services/*"}) ||
		!reflect.DeepEqual(result.Policy.Ignore, []string{"tmp/"}) ||
		!reflect.DeepEqual(result.Policy.ProjectIgnore, map[string][]string{"apps/api": {"generated/"}}) ||
		result.Policy.MaxEntriesPerProject != 42 {
		t.Fatalf("policy = %#v", result.Policy)
	}

	_, err = (GenericSelector{
		StartDir: root,
		Home:     t.TempDir(),
		Config:   config.WorkspaceConfig{Selectors: []string{"../*"}},
	}).Load()
	if err == nil || !strings.Contains(err.Error(), "selector") {
		t.Fatalf("unsafe selector error = %v", err)
	}
}

func TestManifestLoadsCanonicalProjectsInDeterministicOrder(t *testing.T) {
	root := manifestWorkspace(t, `{
		"projectsDir": "catalog",
		"projects": [
			{"name": "web", "path": "services/web"},
			{"name": "api", "path": "apps/api"}
		],
		"policy": {
			"selectors": ["services/*", "apps/*"],
			"ignore": ["tmp/"],
			"projectIgnore": {"apps/api": ["generated/"]},
			"maxEntriesPerProject": 123,
			"maxBytesPerProject": 456
		}
	}`, "")
	mkdirs(t, filepath.Join(root, "catalog"), "apps/api", "services/web")

	result, err := NewManifest(ManifestOptions{Root: root}).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []scan.ProjectDescriptor{
		{ID: "api", Path: "catalog/apps/api", Kind: scan.ProjectShared},
		{ID: "web", Path: "catalog/services/web", Kind: scan.ProjectShared},
	}
	if !reflect.DeepEqual(result.Projects, want) {
		t.Fatalf("projects = %#v, want %#v", result.Projects, want)
	}
	if result.Adapter != scan.SourceAdapterWorkspaceManifest ||
		result.Policy.MaxEntriesPerProject != 123 ||
		result.Policy.MaxBytesPerProject != 456 {
		t.Fatalf("result = %#v", result)
	}
	proposal, err := scan.Scan(result.ScanOptions())
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Source.Adapter != scan.SourceAdapterWorkspaceManifest ||
		proposal.Policy.MaxEntriesPerProject != 123 ||
		proposal.Policy.MaxBytesPerProject != 456 ||
		!reflect.DeepEqual(proposal.Policy.ProjectIgnore, map[string][]string{"catalog/apps/api": {"generated/"}}) {
		t.Fatalf("scan proposal did not preserve source policy: %#v", proposal.Policy)
	}
}

func TestManifestAppliesOptionalLocalProjectOverlay(t *testing.T) {
	root := manifestWorkspace(t, `{
		"projects": [
			{"name": "api", "path": "apps/api"},
			{"name": "web", "path": "services/web"}
		]
	}`, `{
		"projects": [
			{"name": "api", "path": "apps/api-local"},
			{"name": "tooling", "path": "shared/tooling"}
		]
	}`)
	mkdirs(t, filepath.Join(root, "projects"), "apps/api-local", "services/web", "shared/tooling")

	result, err := NewManifest(ManifestOptions{Root: root}).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []scan.ProjectDescriptor{
		{ID: "api", Path: "projects/apps/api-local", Kind: scan.ProjectLocal},
		{ID: "tooling", Path: "projects/shared/tooling", Kind: scan.ProjectLocal},
		{ID: "web", Path: "projects/services/web", Kind: scan.ProjectShared},
	}
	if !reflect.DeepEqual(result.Projects, want) {
		t.Fatalf("projects = %#v, want %#v", result.Projects, want)
	}
}

func TestManifestLocalOverlayCanPreserveCanonicalPath(t *testing.T) {
	root := manifestWorkspace(t, `{
		"projects": [{"name": "api", "path": "apps/api"}]
	}`, `{
		"projects": [{"name": "api"}]
	}`)
	mkdirs(t, filepath.Join(root, "projects"), "apps/api")

	result, err := NewManifest(ManifestOptions{Root: root}).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []scan.ProjectDescriptor{
		{ID: "api", Path: "projects/apps/api", Kind: scan.ProjectLocal},
	}
	if !reflect.DeepEqual(result.Projects, want) {
		t.Fatalf("projects = %#v, want %#v", result.Projects, want)
	}
}

func TestOverlayProjectsShallowMergesNonZeroLocalFields(t *testing.T) {
	canonical := []manifestProject{{
		Name: "api", Path: "apps/api", Repo: "repository-reference",
		Branch: "main", Stack: "service", HubName: "API",
	}}
	local := []manifestProject{{Name: "api", Branch: "feature", HubName: "Local API"}}

	projects, err := overlayProjects(canonical, local)
	if err != nil {
		t.Fatal(err)
	}
	want := manifestProject{
		Name: "api", Path: "apps/api", Repo: "repository-reference",
		Branch: "feature", Stack: "service", HubName: "Local API",
	}
	if len(projects) != 1 || projects[0] != want {
		t.Fatalf("projects = %#v, want %#v", projects, want)
	}
}

func TestOverlayProjectsRejectsDuplicateLocalNames(t *testing.T) {
	_, err := overlayProjects(
		[]manifestProject{{Name: "api", Path: "apps/api"}},
		[]manifestProject{
			{Name: "api", Branch: "first"},
			{Name: "api", Branch: "second"},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "duplicate local project") {
		t.Fatalf("duplicate local project error = %v", err)
	}
}

func TestManifestUsesInjectedEnvironmentRootOverride(t *testing.T) {
	root := manifestWorkspace(t, `{"projects":[{"name":"api","path":"apps/api"}]}`, "")
	external := t.TempDir()
	mkdirs(t, external, "apps/api")
	lookups := []string{}

	result, err := NewManifest(ManifestOptions{
		Root:                  root,
		ApprovedExternalRoots: []string{external},
		LookupEnv: func(name string) (string, bool) {
			lookups = append(lookups, name)
			if name == "WORKSPACE_PROJECTS_DIR" {
				return external, true
			}
			return "", false
		},
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	canonicalExternal, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 || result.Projects[0].Root != filepath.Join(canonicalExternal, "apps/api") {
		t.Fatalf("external project = %#v", result.Projects)
	}
	if !reflect.DeepEqual(result.ApprovedProjectRoots, []string{canonicalExternal}) {
		t.Fatalf("approved roots = %v", result.ApprovedProjectRoots)
	}
	if len(lookups) == 0 {
		t.Fatal("injected environment lookup was not used")
	}
	if _, err := scan.Scan(result.ScanOptions()); err != nil {
		t.Fatalf("scanner rejected approved external project: %v", err)
	}
}

func TestManifestCatalogRootOverridePrecedence(t *testing.T) {
	root := manifestWorkspace(t, `{
		"projectsDir": "manifest-projects",
		"projects": [{"name": "api", "path": "api"}]
	}`, `{"projectsDir":"local-projects"}`)
	external := t.TempDir()
	mkdirs(t, root, "manifest-projects/api", "local-projects/api")
	mkdirs(t, external, "workspace-projects/api", "direct-projects/api")

	result, err := NewManifest(ManifestOptions{
		Root:                  root,
		ApprovedExternalRoots: []string{external},
		LookupEnv: func(name string) (string, bool) {
			switch name {
			case "WORKSPACE_PROJECTS_DIR":
				return filepath.Join(external, "direct-projects"), true
			case "WORKSPACE_ROOT":
				return external, true
			default:
				return "", false
			}
		},
		WorkspaceProjectsSuffix: "workspace-projects",
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	canonicalExternal, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Projects[0].Root; got != filepath.Join(canonicalExternal, "direct-projects", "api") {
		t.Fatalf("direct environment override did not win: %q", got)
	}
}

func TestManifestSupportsCustomWorkspaceRootEnvironmentAndSuffixes(t *testing.T) {
	root := manifestWorkspace(t, `{
		"projectsDir": "catalog",
		"worktreesDir": "trees",
		"projects": [{"name": "api", "path": "nested/api"}],
		"worktreeSets": [{"id":"change-1","path":"change-1","projects":["api"]}]
	}`, "")
	external := t.TempDir()
	mkdirs(t, external, "primary/nested/api", "sets/change-1/api")

	result, err := NewManifest(ManifestOptions{
		Root:                     root,
		ApprovedExternalRoots:    []string{external},
		WorkspaceRootEnv:         "CUSTOM_WORKSPACE_ROOT",
		WorkspaceProjectsSuffix:  "primary",
		WorkspaceWorktreesSuffix: "sets",
		LookupEnv: func(name string) (string, bool) {
			if name == "CUSTOM_WORKSPACE_ROOT" {
				return external, true
			}
			return "", false
		},
		WorktreeSets: []string{"change-1"},
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	canonicalExternal, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	want := []scan.ProjectDescriptor{
		{ID: "api", Path: "projects/nested/api", Kind: scan.ProjectShared, Root: filepath.Join(canonicalExternal, "primary", "nested", "api")},
		{ID: "worktree:change-1:api", Path: "worktrees/change-1/api", Kind: scan.ProjectWorktree, Root: filepath.Join(canonicalExternal, "sets", "change-1", "api")},
	}
	if !reflect.DeepEqual(result.Projects, want) {
		t.Fatalf("projects = %#v, want %#v", result.Projects, want)
	}
}

func TestManifestSupportsCustomDirectCatalogEnvironmentNames(t *testing.T) {
	root := manifestWorkspace(t, `{
		"projects": [{"name":"api","path":"api"}],
		"worktreeSets": [{"id":"change-2","path":"change-2","projects":["api"]}]
	}`, "")
	external := t.TempDir()
	mkdirs(t, external, "primary/api", "sets/change-2/api")

	result, err := NewManifest(ManifestOptions{
		Root:                  root,
		ApprovedExternalRoots: []string{external},
		ProjectsDirEnv:        "CUSTOM_PROJECTS_DIR",
		WorktreesDirEnv:       "CUSTOM_WORKTREES_DIR",
		LookupEnv: func(name string) (string, bool) {
			switch name {
			case "CUSTOM_PROJECTS_DIR":
				return filepath.Join(external, "primary"), true
			case "CUSTOM_WORKTREES_DIR":
				return filepath.Join(external, "sets"), true
			default:
				return "", false
			}
		},
		WorktreeSets: []string{"change-2"},
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 2 || result.Projects[0].Root == "" || result.Projects[1].Root == "" {
		t.Fatalf("custom direct roots were not used: %#v", result.Projects)
	}
}

func TestManifestCatalogOverrideErrorsDoNotExposeHostPaths(t *testing.T) {
	root := manifestWorkspace(t, `{"projects":[{"name":"api","path":"api"}]}`, "")
	external := t.TempDir()
	missing := filepath.Join(external, "missing-projects")

	_, err := NewManifest(ManifestOptions{
		Root:                  root,
		ApprovedExternalRoots: []string{external},
		LookupEnv: func(name string) (string, bool) {
			if name == "WORKSPACE_PROJECTS_DIR" {
				return missing, true
			}
			return "", false
		},
	}).Load()
	if err == nil {
		t.Fatal("Load succeeded with a missing catalog override")
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), external) || strings.Contains(err.Error(), missing) {
		t.Fatalf("catalog override error exposed a host path: %v", err)
	}
}

func TestManifestExcludesWorktreesUnlessExplicitlyRequested(t *testing.T) {
	root := manifestWorkspace(t, `{
		"projects": [{"name": "api", "path": "apps/api"}],
		"worktreeSets": [
			{"id": "review-17", "path": "review-17", "projects": ["api"]}
		]
	}`, "")
	mkdirs(t, filepath.Join(root, "projects"), "apps/api")
	mkdirs(t, filepath.Join(root, "worktrees"), "review-17/api")

	without, err := NewManifest(ManifestOptions{Root: root}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(without.Projects) != 1 {
		t.Fatalf("default projects = %#v", without.Projects)
	}

	with, err := NewManifest(ManifestOptions{Root: root, WorktreeSets: []string{"review-17"}}).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []scan.ProjectDescriptor{
		{ID: "api", Path: "projects/apps/api", Kind: scan.ProjectShared},
		{ID: "worktree:review-17:api", Path: "worktrees/review-17/api", Kind: scan.ProjectWorktree},
	}
	if !reflect.DeepEqual(with.Projects, want) {
		t.Fatalf("projects = %#v, want %#v", with.Projects, want)
	}
}

func TestManifestExplicitlyDiscoversLegacyWorktreeSetFromCanonicalCatalog(t *testing.T) {
	root := manifestWorkspace(t, `{
		"projects": [
			{"name": "api", "path": "apps/api"},
			{"name": "web", "path": "services/web"}
		]
	}`, "")
	mkdirs(t, filepath.Join(root, "projects"), "apps/api", "services/web")
	mkdirs(t, filepath.Join(root, "worktrees"), "review-18/api")

	result, err := NewManifest(ManifestOptions{Root: root, WorktreeSets: []string{"review-18"}}).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []scan.ProjectDescriptor{
		{ID: "api", Path: "projects/apps/api", Kind: scan.ProjectShared},
		{ID: "web", Path: "projects/services/web", Kind: scan.ProjectShared},
		{ID: "worktree:review-18:api", Path: "worktrees/review-18/api", Kind: scan.ProjectWorktree},
	}
	if !reflect.DeepEqual(result.Projects, want) {
		t.Fatalf("projects = %#v, want %#v", result.Projects, want)
	}
}

func TestManifestRejectsMalformedJSONAndUnknownVersion(t *testing.T) {
	for name, content := range map[string]string{
		"malformed":       `{"projects":`,
		"unknown version": `{"formatVersion":99,"projects":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := manifestWorkspace(t, content, "")
			_, err := NewManifest(ManifestOptions{Root: root}).Load()
			if err == nil {
				t.Fatal("Load succeeded")
			}
		})
	}
}

func TestManifestRejectsUnsafeDuplicateAndNestedProjects(t *testing.T) {
	cases := map[string]struct {
		manifest string
		setup    func(t *testing.T, root string)
		want     string
	}{
		"path escape": {
			manifest: `{"projects":[{"name":"api","path":"../outside"}]}`,
			want:     "path",
		},
		"duplicate ID": {
			manifest: `{"projects":[{"name":"api","path":"apps/api"},{"name":"api","path":"services/api"}]}`,
			setup: func(t *testing.T, root string) {
				mkdirs(t, filepath.Join(root, "projects"), "apps/api", "services/api")
			},
			want: "duplicate",
		},
		"duplicate path": {
			manifest: `{"projects":[{"name":"api","path":"apps/api"},{"name":"web","path":"apps/api"}]}`,
			setup: func(t *testing.T, root string) {
				mkdirs(t, filepath.Join(root, "projects"), "apps/api")
			},
			want: "duplicate",
		},
		"nested roots": {
			manifest: `{"projects":[{"name":"api","path":"apps/api"},{"name":"module","path":"apps/api/module"}]}`,
			setup: func(t *testing.T, root string) {
				mkdirs(t, filepath.Join(root, "projects"), "apps/api/module")
			},
			want: "nested",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := manifestWorkspace(t, tc.manifest, "")
			if tc.setup != nil {
				tc.setup(t, root)
			}
			_, err := NewManifest(ManifestOptions{Root: root}).Load()
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestManifestRejectsSymlinkEscapeAndUnapprovedExternalRoot(t *testing.T) {
	root := manifestWorkspace(t, `{"projects":[{"name":"api","path":"apps/api"}]}`, "")
	outside := t.TempDir()
	mkdirs(t, outside, "api")
	if err := os.MkdirAll(filepath.Join(root, "projects", "apps"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "api"), filepath.Join(root, "projects", "apps", "api")); err != nil {
		t.Fatal(err)
	}
	_, err := NewManifest(ManifestOptions{Root: root}).Load()
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink error = %v", err)
	}

	_, err = NewManifest(ManifestOptions{
		Root: root,
		LookupEnv: func(name string) (string, bool) {
			if name == "WORKSPACE_PROJECTS_DIR" {
				return outside, true
			}
			return "", false
		},
	}).Load()
	if err == nil || !strings.Contains(err.Error(), "approved") {
		t.Fatalf("external root error = %v", err)
	}
}

func TestManifestOpensOnlyAllowlistedMetadata(t *testing.T) {
	root := manifestWorkspace(t, `{"projects":[{"name":"api","path":"apps/api"}]}`, `{}`)
	mkdirs(t, filepath.Join(root, "projects"), "apps/api")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	opened := []string{}
	opener := func(path string) (io.ReadCloser, error) {
		relative, err := filepath.Rel(canonicalRoot, path)
		if err != nil {
			t.Fatal(err)
		}
		relative = filepath.ToSlash(relative)
		opened = append(opened, relative)
		switch relative {
		case "manifest/projects.json", ".workspace.local.json":
			return os.Open(path)
		default:
			return nil, errors.New("unexpected read")
		}
	}
	_, err = NewManifest(ManifestOptions{Root: root, OpenFile: opener}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opened, []string{"manifest/projects.json", ".workspace.local.json"}) {
		t.Fatalf("opened files = %v", opened)
	}
}

func TestManifestRejectsSymlinkedMetadataBeforeAnyOpen(t *testing.T) {
	for _, name := range []string{"canonical", "canonical parent", "local"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			outsideDir := t.TempDir()
			outside := filepath.Join(outsideDir, "neutral.json")
			if err := os.WriteFile(outside, []byte(`{"formatVersion":1,"projects":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			canonicalPath := filepath.Join(root, "manifest", "projects.json")
			localPath := filepath.Join(root, ".workspace.local.json")
			if name == "canonical parent" {
				if err := os.WriteFile(filepath.Join(outsideDir, "projects.json"), []byte(`{"formatVersion":1,"projects":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsideDir, filepath.Join(root, "manifest")); err != nil {
					t.Fatal(err)
				}
			} else if name == "canonical" {
				if err := os.MkdirAll(filepath.Join(root, "manifest"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, canonicalPath); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(root, "manifest"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(canonicalPath, []byte(`{"formatVersion":1,"projects":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, localPath); err != nil {
					t.Fatal(err)
				}
			}
			var opened []string
			_, err := NewManifest(ManifestOptions{
				Root: root,
				OpenFile: func(path string) (io.ReadCloser, error) {
					opened = append(opened, path)
					return os.Open(path)
				},
			}).Load()
			if err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("Load() error = %v", err)
			}
			if len(opened) != 0 {
				t.Fatalf("opener called for rejected metadata symlink: %v", opened)
			}
			if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), outside) {
				t.Fatalf("error exposed host path: %v", err)
			}
		})
	}
}

func TestManifestFailsClosedWhenOptionalLocalMetadataCannotBeRead(t *testing.T) {
	root := manifestWorkspace(t, `{"projects":[{"name":"api","path":"apps/api"}]}`, "")
	mkdirs(t, filepath.Join(root, "projects"), "apps/api")
	opener := func(path string) (io.ReadCloser, error) {
		if filepath.Base(path) == ".workspace.local.json" {
			return nil, fs.ErrPermission
		}
		return os.Open(path)
	}
	_, err := NewManifest(ManifestOptions{Root: root, OpenFile: opener}).Load()
	if err == nil {
		t.Fatal("Load succeeded despite unreadable local metadata")
	}
}

func manifestWorkspace(t *testing.T, manifest, local string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "manifest"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest", "projects.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if local != "" {
		if err := os.WriteFile(filepath.Join(root, ".workspace.local.json"), []byte(local), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mkdirs(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(path)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}
