// Proprietary and confidential. All rights reserved.

package workspace

import (
	"os"
	"path/filepath"
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
