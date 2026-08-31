package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestRepositoryCatalogCapturesOrgFromOriginRemote(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "apps/api")
	mkdirRepository(t, root, "tools/cli")
	initGitOrigin(t, filepath.Join(root, "apps", "api"), "https://github.com/acme/api.git")
	initGitOrigin(t, filepath.Join(root, "tools", "cli"), "git@gitlab.com:acme/cli.git")

	result, err := NewRepositoryCatalog(RepositoryOptions{Root: root}).Load()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]scan.ProjectDescriptor{}
	for _, project := range result.Projects {
		byID[project.ID] = project
	}
	if byID["apps/api"].Org != "acme" {
		t.Fatalf("apps/api org = %q, want acme", byID["apps/api"].Org)
	}
	if byID["tools/cli"].Org != "" {
		t.Fatalf("non-github org = %q, want empty", byID["tools/cli"].Org)
	}
}

func initGitOrigin(t *testing.T, dir, url string) {
	t.Helper()
	cmd := exec.Command("git", "init", "--quiet", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmd = exec.Command("git", "-C", dir, "remote", "add", "origin", url)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
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
		"depth":        {MaxDepth: 1, MaxDirectories: 100, MaxRepositories: 100},
		"directories":  {MaxDepth: 10, MaxDirectories: 1, MaxRepositories: 100},
		"repositories": {MaxDepth: 10, MaxDirectories: 100, MaxRepositories: 1},
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
			if !strings.Contains(err.Error(), "1") {
				t.Fatalf("error = %q, want limit value", err)
			}
			if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), os.TempDir()) {
				t.Fatalf("error exposed host path: %v", err)
			}
		})
	}
}

func TestRepositoryCatalogTraversesDirectoriesHoldingManyPlainFiles(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "project")
	mkdirs(t, root, "project/data")
	writePlainFiles(t, filepath.Join(root, "project", "data"), 512)
	mkdirRepository(t, root, "archive/library")

	// The directory budget is exactly the fixture's directory count, so any
	// bound that counts plain files would abort this walk.
	result, err := NewRepositoryCatalog(RepositoryOptions{
		Root: root, MaxDepth: 10, MaxDirectories: 5, MaxRepositories: 10,
	}).Load()
	if err != nil {
		t.Fatalf("plain files aborted the repository walk: %v", err)
	}
	paths := make([]string, 0, len(result.Projects))
	for _, project := range result.Projects {
		paths = append(paths, project.Path)
	}
	if !reflect.DeepEqual(paths, []string{"archive/library", "project"}) {
		t.Fatalf("projects = %#v", paths)
	}
}

func TestRepositoryCatalogFailsClosedOnSubdirectoryFanOut(t *testing.T) {
	root := t.TempDir()
	mkdirRepository(t, root, "project")
	for index := 0; index < 32; index++ {
		mkdirs(t, root, "wide/branch-"+strconv.Itoa(index))
	}

	result, err := NewRepositoryCatalog(RepositoryOptions{
		Root: root, MaxDepth: 10, MaxDirectories: 8, MaxRepositories: 10,
	}).Load()
	if err == nil {
		t.Fatal("Load succeeded despite subdirectory fan-out")
	}
	if !strings.Contains(err.Error(), "directories") || !strings.Contains(err.Error(), "8") {
		t.Fatalf("error = %q, want the bound name and its limit", err)
	}
	if !strings.Contains(err.Error(), "wide/branch-") {
		t.Fatalf("error = %q, want the workspace-relative path where the bound tripped", err)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), os.TempDir()) {
		t.Fatalf("error exposed host path: %v", err)
	}
	if len(result.Projects) != 0 {
		t.Fatalf("bounded walk returned a partial catalog: %#v", result.Projects)
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

func writePlainFiles(t *testing.T, directory string, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		name := filepath.Join(directory, "record-"+strconv.Itoa(index)+".html")
		if err := os.WriteFile(name, []byte("<html></html>"), 0o600); err != nil {
			t.Fatal(err)
		}
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
