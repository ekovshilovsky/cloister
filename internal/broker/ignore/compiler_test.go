// Proprietary and confidential. All rights reserved.

package ignore

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCompileMatchesGitCheckIgnoreSemantics(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for ignore conformance")
	}
	source := t.TempDir()
	compiledRoot := t.TempDir()
	initGit(t, source)
	initGit(t, compiledRoot)

	writeFixture(t, source, ".gitignore", "*.log\n/root-only\ncache/\nvendor/\n!keep.log\n")
	writeFixture(t, source, "services/api/.gitignore", "generated/\n/local.txt\n*.tmp\n!keep.tmp\n")
	writeFixture(t, source, "vendor/.gitignore", "!keep.txt\n")
	paths := []string{
		"error.log",
		"keep.log",
		"nested/error.log",
		"root-only",
		"nested/root-only",
		"cache/data.txt",
		"services/api/generated/code.go",
		"services/api/nested/generated/code.go",
		"services/web/generated/code.go",
		"services/api/local.txt",
		"services/api/nested/local.txt",
		"services/api/nested/value.tmp",
		"services/api/nested/keep.tmp",
		"vendor/keep.txt",
	}
	for _, path := range paths {
		writeFixture(t, source, path, "fixture")
		writeFixture(t, compiledRoot, path, "fixture")
	}

	policy, err := Compile(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	compiled := strings.Join(policy.RepositoryStrings(), "\n") + "\n"
	writeFixture(t, compiledRoot, ".gitignore", compiled)

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			want := gitIgnored(t, source, path)
			got := gitIgnored(t, compiledRoot, path)
			if got != want {
				t.Fatalf("compiled policy mismatch for %q: got ignored=%v want=%v\ncompiled:\n%s", path, got, want, compiled)
			}
		})
	}
}

func TestCompileAppendsMandatoryPatternsLast(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, ".gitignore", "!.git\n!node_modules/\n")
	policy, err := Compile(root, []string{"!build/", ".private/"})
	if err != nil {
		t.Fatal(err)
	}
	patterns := policy.Strings()
	mandatory := MandatoryPatterns()
	if len(patterns) < len(mandatory) {
		t.Fatalf("got %d patterns, need at least %d", len(patterns), len(mandatory))
	}
	if got := patterns[len(patterns)-len(mandatory):]; !reflect.DeepEqual(got, mandatory) {
		t.Fatalf("final patterns = %v, want mandatory %v", got, mandatory)
	}
	if !policy.Ignored(".git/config", false) || !policy.Ignored("packages/app/node_modules/pkg/index.js", false) {
		t.Fatal("mandatory exclusions were negated")
	}
}

func TestCompileFailsClosedOnUnrepresentableRules(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, ".gitignore", "safe/\n{secret,private}/\n")
	_, err := Compile(root, nil)
	if err == nil || !strings.Contains(err.Error(), ".gitignore:2") || !strings.Contains(err.Error(), "cannot be represented safely") {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestNestedRulesAreRebased(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "services/api/.gitignore", "*.tmp\n/local.txt\ngenerated/\n")
	policy, err := Compile(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/services/api/**/*.tmp",
		"/services/api/local.txt",
		"/services/api/**/generated/",
	}
	got := policy.RepositoryStrings()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patterns = %#v, want %#v", got, want)
	}
}

func initGit(t *testing.T, root string) {
	t.Helper()
	cmd := exec.Command("git", "init", "--quiet", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func writeFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitIgnored(t *testing.T, root, relative string) bool {
	t.Helper()
	cmd := exec.Command("git", "-c", "core.excludesFile=/dev/null", "-C", root, "check-ignore", "--no-index", "--verbose", "--", relative)
	_, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git check-ignore %q: %v", relative, err)
	return false
}
