package layout

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const githubHost = "github.com"

// ParseGitHubOrg returns the GitHub organization from a git remote URL.
// It understands https://github.com/ORG/REPO(.git) and git@github.com:ORG/REPO(.git)
// plus the common ssh:// and git:// github.com forms. Non-GitHub or unparseable
// values return the empty string.
func ParseGitHubOrg(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if org := parseGitHubSCPOrg(raw); org != "" || looksLikeGitHubSCP(raw) {
		return org
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !strings.EqualFold(parsed.Hostname(), githubHost) {
		return ""
	}
	return orgFromPath(parsed.Path)
}

func looksLikeGitHubSCP(raw string) bool {
	return strings.Contains(strings.ToLower(raw), "@"+githubHost+":")
}

func parseGitHubSCPOrg(raw string) string {
	lower := strings.ToLower(raw)
	marker := "@" + githubHost + ":"
	index := strings.Index(lower, marker)
	if index < 0 {
		return ""
	}
	return orgFromPath(raw[index+len(marker):])
}

func orgFromPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	if !safeOrgSegment(parts[0]) {
		return ""
	}
	return parts[0]
}

func safeOrgSegment(org string) bool {
	if org == "" || org == "." || org == ".." {
		return false
	}
	for _, r := range org {
		if !(r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return false
		}
	}
	return true
}

// OriginOrg reads the host-side origin remote for a repository root and
// returns its GitHub organization. Missing remotes, non-GitHub hosts, and
// unreadable metadata produce the empty string. The function does not read
// project file contents.
func OriginOrg(projectRoot string) string {
	if projectRoot == "" {
		return ""
	}
	if _, err := os.Lstat(filepath.Join(projectRoot, ".git")); err != nil {
		return ""
	}
	cmd := exec.Command("git", "-C", projectRoot, "config", "--get", "remote.origin.url")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return ParseGitHubOrg(string(out))
}
