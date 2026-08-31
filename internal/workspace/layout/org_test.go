package layout

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseGitHubOrg(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "https with git suffix", raw: "https://github.com/1-800-Battery/AWSCrossReference.git", want: "1-800-Battery"},
		{name: "https without git suffix", raw: "https://github.com/1-800-Battery/AWSCrossReference", want: "1-800-Battery"},
		{name: "https with trailing slash", raw: "https://github.com/acme/repo.git/", want: "acme"},
		{name: "http", raw: "http://github.com/acme/repo.git", want: "acme"},
		{name: "ssh scp form", raw: "git@github.com:1-800-Battery/AWSCrossReference.git", want: "1-800-Battery"},
		{name: "ssh scp form without git suffix", raw: "git@github.com:acme/repo", want: "acme"},
		{name: "ssh scp mixed case host", raw: "GIT@GITHUB.COM:AcmeOrg/repo.git", want: "AcmeOrg"},
		{name: "ssh url", raw: "ssh://git@github.com/acme/repo.git", want: "acme"},
		{name: "ssh url without user", raw: "ssh://github.com/acme/repo.git", want: "acme"},
		{name: "git protocol", raw: "git://github.com/acme/repo.git", want: "acme"},
		{name: "empty", raw: "", want: ""},
		{name: "whitespace only", raw: "  \n\t", want: ""},
		{name: "gitlab https", raw: "https://gitlab.com/acme/repo.git", want: ""},
		{name: "gitlab ssh", raw: "git@gitlab.com:acme/repo.git", want: ""},
		{name: "missing repo segment", raw: "https://github.com/acme", want: ""},
		{name: "missing repo scp", raw: "git@github.com:acme", want: ""},
		{name: "gist subdomain", raw: "https://gist.github.com/acme/abc123", want: ""},
		{name: "not a url", raw: "not a git remote", want: ""},
		{name: "file url", raw: "/tmp/local-repo.git", want: ""},
		{name: "org with hyphen and digits", raw: "https://github.com/org-123/repo.git", want: "org-123"},
		{name: "extra path ignored", raw: "https://github.com/acme/repo/tree/main", want: "acme"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ParseGitHubOrg(test.raw); got != test.want {
				t.Fatalf("ParseGitHubOrg(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

func TestOriginOrgReadsGitHubRemote(t *testing.T) {
	root := t.TempDir()
	if OriginOrg(root) != "" {
		t.Fatal("OriginOrg() on a non-repository must be empty")
	}
	initGit(t, root)
	if OriginOrg(root) != "" {
		t.Fatal("OriginOrg() without origin must be empty")
	}
	addOrigin(t, root, "https://github.com/acme/repo.git")
	if got := OriginOrg(root); got != "acme" {
		t.Fatalf("OriginOrg() = %q, want acme", got)
	}
	addOrigin(t, root, "git@gitlab.com:acme/repo.git")
	if got := OriginOrg(root); got != "" {
		t.Fatalf("OriginOrg() gitlab remote = %q, want empty", got)
	}
}

func initGit(t *testing.T, root string) {
	t.Helper()
	cmd := exec.Command("git", "init", "--quiet", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func addOrigin(t *testing.T, root, url string) {
	t.Helper()
	if err := exec.Command("git", "-C", root, "remote", "remove", "origin").Run(); err != nil {
		// First assignment has no origin yet.
		_ = err
	}
	cmd := exec.Command("git", "-C", root, "remote", "add", "origin", url)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
}

func TestOriginOrgIgnoresMissingGit(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "project"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := OriginOrg(filepath.Join(root, "project")); got != "" {
		t.Fatalf("OriginOrg() = %q, want empty", got)
	}
}
