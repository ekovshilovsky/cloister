package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloister.io/internal/config"
)

func TestResolveProfileChoosesMostSpecificScope(t *testing.T) {
	root := t.TempDir()
	team := filepath.Join(root, "team")
	project := filepath.Join(team, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	profiles := map[string]*config.Profile{
		"broad":  {StartDir: root},
		"narrow": {StartDir: team},
	}

	got, err := ResolveProfile(project, root, profiles)
	if err != nil {
		t.Fatal(err)
	}
	canonicalProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	canonicalTeam, err := filepath.EvalSymlinks(team)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "narrow" || got.Path != canonicalProject || got.Scope != canonicalTeam {
		t.Fatalf("ResolveProfile() = %#v", got)
	}
}

func TestResolveProfileNoMatchListsCandidates(t *testing.T) {
	home := t.TempDir()
	one := filepath.Join(home, "one")
	two := filepath.Join(home, "two")
	outside := filepath.Join(home, "outside")
	for _, path := range []string{one, two, outside} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	profiles := map[string]*config.Profile{
		"zeta":  {StartDir: two},
		"alpha": {StartDir: one},
	}

	_, err := ResolveProfile(outside, home, profiles)
	if err == nil {
		t.Fatal("expected no-match error")
	}
	message := err.Error()
	if !strings.Contains(message, "not within any configured profile start_dir") ||
		!strings.Contains(message, "alpha ("+one+")") ||
		!strings.Contains(message, "zeta ("+two+")") ||
		strings.Index(message, "alpha (") > strings.Index(message, "zeta (") {
		t.Fatalf("unexpected no-match error: %v", err)
	}
}

func TestResolveProfileCanonicalizesRelativeSymlinkPath(t *testing.T) {
	root := t.TempDir()
	realProject := filepath.Join(root, "real", "project")
	if err := os.MkdirAll(realProject, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "real"), alias); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, filepath.Join(alias, "project", "..", "project"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := ResolveProfile(relative, root, map[string]*config.Profile{
		"work": {StartDir: filepath.Join(root, "real")},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonicalProject, err := filepath.EvalSymlinks(realProject)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != canonicalProject {
		t.Fatalf("canonical path = %q, want %q", got.Path, canonicalProject)
	}
}
