// Proprietary and confidential. All rights reserved.

package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cloister.io/internal/broker"
	"cloister.io/internal/config"
	"cloister.io/internal/vm"
)

func TestDiscoverBuildsWholeProjectSessionsWithMinimalIgnores(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"apps/api", "tools/rockauto-scraper"} {
		if err := os.MkdirAll(filepath.Join(root, project), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "apps/api", "untracked.local"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "apps/api", ".gitignore"), []byte("dist/\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	specs, err := Discover("work", root, t.TempDir(), config.WorkspaceConfig{
		MaxEntryCount:      123_456,
		MaxStagingFileSize: "512 MiB",
		ProjectIgnore: map[string][]string{
			"apps/api":               {".local-generated/"},
			"tools/rockauto-scraper": {"data/raw/"},
		},
	}, vm.SSHAccess{Host: "vm.local", User: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("sessions = %d, want 2", len(specs))
	}

	byBase := map[string]broker.SessionSpec{}
	for _, spec := range specs {
		byBase[filepath.Base(spec.HostRoot)] = spec
		if spec.MaxEntries != 123_456 || spec.MaxStagingFileSize != "512 MiB" || spec.ProbeMode != "assume" {
			t.Fatalf("guardrails for %q = %#v", spec.HostRoot, spec)
		}
		if strings.Contains(spec.GuestRoot, root) || !strings.HasPrefix(spec.GuestRoot, "~/workspaces/") {
			t.Fatalf("unsafe guest root %q", spec.GuestRoot)
		}
		policy, err := broker.CompilePolicy(spec)
		if err != nil {
			t.Fatal(err)
		}
		patterns := policy.Strings()
		if !containsString(patterns, ".git") || !containsString(patterns, "node_modules/") {
			t.Fatalf("minimal mandatory ignores missing: %v", patterns)
		}
		for _, broad := range []string{"build/", "dist/", "coverage/", ".venv/"} {
			if containsString(patterns, broad) {
				t.Fatalf("workspace policy over-filtered %q: %v", broad, patterns)
			}
		}
	}
	if !containsString(byBase["api"].Ignore, ".local-generated/") {
		t.Fatalf("per-project ignore not applied: %v", byBase["api"].Ignore)
	}
	if !containsString(byBase["rockauto-scraper"].Ignore, "data/raw/") {
		t.Fatalf("per-project ignore for rockauto-scraper not applied: %v", byBase["rockauto-scraper"].Ignore)
	}
}

func TestDiscoverGuestRootsAreCollisionSafe(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"apps/api", "tools/api"} {
		if err := os.MkdirAll(filepath.Join(root, project), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	specs, err := Discover("work", root, root, config.WorkspaceConfig{}, vm.SSHAccess{})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].GuestRoot == specs[1].GuestRoot || specs[0].ProjectID == specs[1].ProjectID {
		t.Fatalf("colliding sessions: %#v", specs)
	}
}

func TestDiscoverRejectsSourceRootSelectorWithoutRepository(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Discover("sandbox", root, t.TempDir(), config.WorkspaceConfig{
		Selectors: []string{"."},
	}, vm.SSHAccess{})
	if err == nil || !strings.Contains(err.Error(), `selector "."`) ||
		!strings.Contains(err.Error(), "repository root") {
		t.Fatalf("Discover() error = %v", err)
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("error exposed host path: %v", err)
	}
}

func TestDiscoverSupportsSoleSourceRootSelectorForRepository(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs, err := Discover("sandbox", root, t.TempDir(), config.WorkspaceConfig{
		Selectors: []string{"."},
	}, vm.SSHAccess{})
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].HostRoot != canonicalRoot {
		t.Fatalf("root project sessions = %#v", specs)
	}
}

func TestDiscoverRejectsSourceRootSelectorAlongsideChildren(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "apps", "api"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Discover("sandbox", root, t.TempDir(), config.WorkspaceConfig{
		Selectors: []string{".", "apps/*"},
	}, vm.SSHAccess{})
	if err == nil || !strings.Contains(err.Error(), "keep either the root or its children, not both") {
		t.Fatalf("Discover() error = %v", err)
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("error exposed host path: %v", err)
	}
}

func TestBuildProjectSpecGatesSourceRootProject(t *testing.T) {
	root := t.TempDir()
	_, err := BuildProjectSpec(
		"sandbox",
		root,
		root,
		config.WorkspaceConfig{Selectors: []string{"."}},
		vm.SSHAccess{},
	)
	if err == nil || !strings.Contains(err.Error(), "repository root") {
		t.Fatalf("BuildProjectSpec() error = %v", err)
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("error exposed host path: %v", err)
	}
}

func TestDiscoverRejectsUnusedProjectIgnore(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "apps/api"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Discover("work", root, root, config.WorkspaceConfig{
		ProjectIgnore: map[string][]string{"apps/typo": {"output/"}},
	}, vm.SSHAccess{})
	if err == nil || !strings.Contains(err.Error(), "does not name a selected project") {
		t.Fatalf("Discover() error = %v", err)
	}
}

func TestProjectSessionMatchesCollectionActivation(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"apps/api", "tools/scraper"} {
		if err := os.MkdirAll(filepath.Join(root, project), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	access := vm.SSHAccess{Host: "vm.local", User: "guest"}
	base := config.WorkspaceConfig{
		Root:   root,
		Ignore: []string{"tmp/"},
		ProjectIgnore: map[string][]string{
			"apps/api":      {".local-generated/"},
			"tools/scraper": {"data/raw/"},
		},
	}
	overridden := base
	overridden.MaxEntryCount = 123_456
	overridden.MaxStagingFileSize = "512 MiB"

	cases := map[string]struct {
		cfg                config.WorkspaceConfig
		maxEntries         uint64
		maxStagingFileSize string
	}{
		"guardrail defaults":  {cfg: base, maxEntries: DefaultMaxEntryCount, maxStagingFileSize: DefaultMaxStagingFileSize},
		"guardrail overrides": {cfg: overridden, maxEntries: 123_456, maxStagingFileSize: "512 MiB"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			specs, err := Discover("work", root, home, tc.cfg, access)
			if err != nil {
				t.Fatal(err)
			}
			var want broker.SessionSpec
			for _, spec := range specs {
				if filepath.Base(spec.HostRoot) == "api" {
					want = spec
				}
			}
			if want.HostRoot == "" {
				t.Fatalf("collection activation produced no session for apps/api: %#v", specs)
			}

			got, err := ProjectSession("work", filepath.Join(root, "apps", "api"), root, home, tc.cfg, access)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ProjectSession() = %#v, want %#v", got, want)
			}
			if got.MaxEntries != tc.maxEntries || got.MaxStagingFileSize != tc.maxStagingFileSize {
				t.Fatalf("guardrails = %d/%q", got.MaxEntries, got.MaxStagingFileSize)
			}
			if got.ProbeMode != "assume" || !got.SkipGitignores {
				t.Fatalf("probe mode %q, skip gitignores %v", got.ProbeMode, got.SkipGitignores)
			}
			if !reflect.DeepEqual(got.MandatoryIgnore, minimalMandatoryIgnore) {
				t.Fatalf("mandatory ignores = %v", got.MandatoryIgnore)
			}
			if !containsString(got.Ignore, "tmp/") || !containsString(got.Ignore, ".local-generated/") {
				t.Fatalf("ignores = %v", got.Ignore)
			}
			if containsString(got.Ignore, "data/raw/") {
				t.Fatalf("ignores leaked another project's entry: %v", got.Ignore)
			}
		})
	}
}

func TestProjectSessionRejectsPathsOutsideTheSelectedSet(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for _, dir := range []string{"apps/api", "vendor/library", "apps/api/internal"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.WorkspaceConfig{Root: root}

	cases := map[string]struct {
		path string
		want string
	}{
		"outside the root":       {path: outside, want: "outside the workspace root"},
		"the root itself":        {path: root, want: "not selected"},
		"unselected sibling":     {path: filepath.Join(root, "vendor", "library"), want: "not selected"},
		"nested below a project": {path: filepath.Join(root, "apps", "api", "internal"), want: "not selected"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ProjectSession("work", tc.path, root, t.TempDir(), cfg, vm.SSHAccess{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ProjectSession() error = %v, want it to mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), outside) {
				t.Fatalf("ProjectSession() error exposed host path: %v", err)
			}
		})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
