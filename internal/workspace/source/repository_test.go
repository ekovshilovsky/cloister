package source

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cloister.io/internal/config"
	"cloister.io/internal/workspace/scan"
)

func TestRepositoryCatalogDiscoversCanonicalWorktreeAndNestedRepositories(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, ".")
	mkdirRepository(t, root, "groups/outer")
	mkdirRepository(t, root, "groups/outer/modules/inner")
	mkdirWorktree(t, root, "trees/change")
	mkdirs(t, root, "scratch", "node_modules/ignored", "dist/ignored")
	mkdirRepository(t, root, "node_modules/ignored")
	mkdirRepository(t, root, "dist/ignored")

	outside := t.TempDir()
	mkdirRepository(t, outside, "linked")
	if err := os.Symlink(filepath.Join(outside, "linked"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "groups", "outer", ".git"), filepath.Join(root, "scratch", ".git")); err != nil {
		t.Fatal(err)
	}

	result, err := NewRepositoryCatalog(RepositoryOptions{Root: root}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if result.Adapter != scan.SourceAdapterRepository {
		t.Fatalf("adapter = %q", result.Adapter)
	}
	want := []scan.ProjectDescriptor{
		{
			ID: ".", Path: ".", Kind: scan.ProjectRepository,
			NestedRepositories: 3, Reason: "contains 3 nested repositories; synchronizing it would overlap them",
			Recommendation: scan.RecommendationReview, Decision: scan.DecisionReview,
		},
		{
			ID: "groups/outer", Path: "groups/outer", Kind: scan.ProjectRepository,
			NestedRepositories: 1, Reason: "contains 1 nested repository; synchronizing it would overlap it",
			Recommendation: scan.RecommendationReview, Decision: scan.DecisionReview,
		},
		{
			ID: "groups/outer/modules/inner", Path: "groups/outer/modules/inner", Kind: scan.ProjectRepository,
			Reason: "canonical repository", Recommendation: scan.RecommendationInclude, Decision: scan.DecisionInclude,
		},
		{
			ID: "trees/change", Path: "trees/change", Kind: scan.ProjectWorktree,
			Reason: "git worktree checkout", Recommendation: scan.RecommendationInclude, Decision: scan.DecisionInclude,
		},
	}
	if !reflect.DeepEqual(result.Projects, want) {
		t.Fatalf("projects = %#v, want %#v", result.Projects, want)
	}
	if result.Policy.MaxEntriesPerProject != scan.DefaultMaxEntriesPerProject ||
		result.Policy.MaxBytesPerProject != scan.DefaultMaxBytesPerProject {
		t.Fatalf("default policy limits = %#v", result.Policy)
	}
}

func TestRepositoryCatalogPrunesInfrastructureCaches(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "project")
	for _, cache := range []string{".terraform", ".terragrunt-cache"} {
		mkdirRepository(t, root, "project/"+cache+"/cached-provider")
	}

	result, err := NewRepositoryCatalog(RepositoryOptions{Root: root}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 || result.Projects[0].Path != "project" {
		t.Fatalf("cache repositories were discovered: %#v", result.Projects)
	}
}

func TestRepositoryCatalogRetainsSelectorsWithoutLimitingDiscovery(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "apps/existing")
	mkdirRepository(t, root, "new/deep/repository")

	result, err := NewRepositoryCatalog(RepositoryOptions{
		Root: root,
		Config: config.WorkspaceConfig{
			Selectors:     []string{"apps/existing"},
			Ignore:        []string{"temporary/"},
			ProjectIgnore: map[string][]string{"apps/existing": {"generated/"}},
			MaxEntryCount: 321,
		},
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 2 {
		t.Fatalf("projects = %#v, selectors hid a repository", result.Projects)
	}
	for _, project := range result.Projects {
		if project.Decision != scan.DecisionInclude {
			t.Fatalf("project %q decision = %q", project.Path, project.Decision)
		}
	}
	if !reflect.DeepEqual(result.Policy.Selectors, []string{"apps/existing"}) ||
		!reflect.DeepEqual(result.Policy.Ignore, []string{"temporary/"}) ||
		!reflect.DeepEqual(result.Policy.ProjectIgnore, map[string][]string{"apps/existing": {"generated/"}}) ||
		result.Policy.MaxEntriesPerProject != 321 {
		t.Fatalf("policy = %#v", result.Policy)
	}
}

func TestRepositoryCatalogKeepsSelectedParentAtReviewWhenNewNestedRepositoryExists(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "outer")
	mkdirRepository(t, root, "outer/new")

	result, err := NewRepositoryCatalog(RepositoryOptions{
		Root: root,
		Config: config.WorkspaceConfig{
			Selectors: []string{"outer"},
		},
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 2 ||
		result.Projects[0].Path != "outer" ||
		result.Projects[0].Decision != scan.DecisionReview {
		t.Fatalf("nested drift did not require parent review: %#v", result.Projects)
	}
}

func TestRepositoryCatalogFailsClosedAtWalkBoundsWithoutHostPaths(t *testing.T) {
	for name, options := range map[string]RepositoryOptions{
		"depth": {MaxDepth: 1, MaxDirectories: 100, MaxDirectoryEntries: 100, MaxRepositories: 100},
		"directories": {
			MaxDepth: 10, MaxDirectories: 1, MaxDirectoryEntries: 100, MaxRepositories: 100,
		},
		"entries": {
			MaxDepth: 10, MaxDirectories: 100, MaxDirectoryEntries: 2, MaxRepositories: 100,
		},
		"repositories": {
			MaxDepth: 10, MaxDirectories: 100, MaxDirectoryEntries: 100, MaxRepositories: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			mkdirRepository(t, root, "one")
			mkdirRepository(t, root, "nested/two")
			options.Root = root

			_, err := NewRepositoryCatalog(options).Load()
			if err == nil {
				t.Fatal("Load succeeded despite repository walk bound")
			}
			boundName := name
			if name == "repositories" {
				boundName = "repository"
			}
			if !strings.Contains(err.Error(), boundName) {
				t.Fatalf("error = %q, want bound name %q", err, boundName)
			}
			wantLimit := "1"
			if name == "entries" {
				wantLimit = "2"
			}
			if !strings.Contains(err.Error(), wantLimit) {
				t.Fatalf("error = %q, want limit value", err)
			}
			if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), os.TempDir()) {
				t.Fatalf("error exposed host path: %v", err)
			}
		})
	}
}

func TestRepositoryCatalogDoesNotTraverseDirectorySymlinks(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "inside")
	outside := t.TempDir()
	mkdirRepository(t, outside, "outside")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	result, err := NewRepositoryCatalog(RepositoryOptions{Root: root}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 || result.Projects[0].Path != "inside" {
		t.Fatalf("directory symlink was traversed: %#v", result.Projects)
	}
}

func TestRepositoryCatalogRejectsSymlinkSourceRoot(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "real")
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Fatal(err)
	}

	_, err := NewRepositoryCatalog(RepositoryOptions{Root: link}).Load()
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink root error = %v", err)
	}
	if strings.Contains(err.Error(), link) || strings.Contains(err.Error(), root) {
		t.Fatalf("symlink root error exposed host path: %v", err)
	}
}

func TestRepositoryCatalogReadErrorOmitsHostPath(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	_, err := NewRepositoryCatalog(RepositoryOptions{Root: root}).Load()
	if err == nil {
		t.Fatal("Load succeeded despite unreadable directory")
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), os.TempDir()) {
		t.Fatalf("walk error exposed host path: %v", err)
	}
}

func TestRepositoryCatalogRejectsNonPortablePolicySelector(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "project")

	_, err := NewRepositoryCatalog(RepositoryOptions{
		Root: root,
		Config: config.WorkspaceConfig{
			Selectors: []string{filepath.Join(root, "project")},
		},
	}).Load()
	if err == nil || !strings.Contains(err.Error(), "portable relative") {
		t.Fatalf("selector error = %v", err)
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("selector error exposed host path: %v", err)
	}
}

func TestRepositoryCatalogGatesSourceRootSelectorBeforeWalking(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "nested/deep/project")

	_, err := NewRepositoryCatalog(RepositoryOptions{
		Root:     root,
		MaxDepth: 1,
		Config: config.WorkspaceConfig{
			Selectors: []string{"."},
		},
	}).Load()
	if err == nil || !strings.Contains(err.Error(), `selector "."`) ||
		!strings.Contains(err.Error(), "repository root") {
		t.Fatalf("selector error = %v", err)
	}
	if strings.Contains(err.Error(), "depth bound") {
		t.Fatalf("repository walk ran before selector gate: %v", err)
	}
}

func TestRepositoryCatalogInvalidSelectorErrorDoesNotExposeHostPath(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "repository")
	_, err := NewRepositoryCatalog(RepositoryOptions{
		Root: root,
		Config: config.WorkspaceConfig{
			Selectors: []string{root + "["},
		},
	}).Load()
	if err == nil || !strings.Contains(err.Error(), "selector") {
		t.Fatalf("invalid selector error = %v", err)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), os.TempDir()) {
		t.Fatalf("invalid selector error exposed host path: %v", err)
	}
}

func mkdirRepository(t *testing.T, root, relative string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func mkdirWorktree(t *testing.T, root, relative string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: /private/value/must-not-be-read\n"), 0o000); err != nil {
		t.Fatal(err)
	}
}
