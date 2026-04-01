package linux

import (
	"strings"
	"testing"
)

func TestTranslatePath(t *testing.T) {
	hostHome := "/Users/testuser"
	vmHome := "/home/testuser.guest"

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plugin install path",
			input: "/Users/testuser/.claude/plugins/cache/claude-plugins-official/superpowers/5.0.6",
			want:  "/home/testuser.guest/.claude/plugins/cache/claude-plugins-official/superpowers/5.0.6",
		},
		{
			name:  "marketplace install location",
			input: "/Users/testuser/.claude/plugins/marketplaces/claude-plugins-official",
			want:  "/home/testuser.guest/.claude/plugins/marketplaces/claude-plugins-official",
		},
		{
			name:  "unrelated path unchanged",
			input: "/usr/local/bin/something",
			want:  "/usr/local/bin/something",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translatePath(hostHome, vmHome, tt.input)
			if got != tt.want {
				t.Errorf("translatePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTranslateInstalledPlugins(t *testing.T) {
	hostHome := "/Users/testuser"
	vmHome := "/home/testuser.guest"

	input := []byte(`{
		"version": 2,
		"plugins": {
			"superpowers@claude-plugins-official": [{
				"scope": "user",
				"installPath": "/Users/testuser/.claude/plugins/cache/claude-plugins-official/superpowers/5.0.6",
				"version": "5.0.6",
				"installedAt": "2026-03-10T21:26:34.400Z",
				"lastUpdated": "2026-03-25T18:33:36.259Z"
			}]
		}
	}`)

	got, err := TranslateInstalledPlugins(input, hostHome, vmHome)
	if err != nil {
		t.Fatalf("TranslateInstalledPlugins: %v", err)
	}

	if !bytesContain(got, "/home/testuser.guest/.claude/plugins/cache/") {
		t.Errorf("expected VM path in output, got: %s", got)
	}
	if bytesContain(got, "/Users/testuser/") {
		t.Errorf("host path should not appear in output, got: %s", got)
	}
}

func TestTranslateKnownMarketplaces(t *testing.T) {
	hostHome := "/Users/testuser"
	vmHome := "/home/testuser.guest"

	input := []byte(`{
		"claude-plugins-official": {
			"source": {"source": "github", "repo": "anthropics/claude-plugins-official"},
			"installLocation": "/Users/testuser/.claude/plugins/marketplaces/claude-plugins-official",
			"lastUpdated": "2026-03-27T21:21:07.541Z"
		}
	}`)

	got, err := TranslateKnownMarketplaces(input, hostHome, vmHome)
	if err != nil {
		t.Fatalf("TranslateKnownMarketplaces: %v", err)
	}

	if !bytesContain(got, "/home/testuser.guest/.claude/plugins/marketplaces/") {
		t.Errorf("expected VM path in output, got: %s", got)
	}
	if bytesContain(got, "/Users/testuser/") {
		t.Errorf("host path should not appear in output, got: %s", got)
	}
}

func TestTranslateSettings(t *testing.T) {
	hostHome := "/Users/testuser"
	vmHome := "/home/testuser.guest"

	input := []byte(`{
		"env": {
			"PATH": "/Users/testuser/.local/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
			"HOMEBREW_PREFIX": "/opt/homebrew",
			"HOMEBREW_CELLAR": "/opt/homebrew/Cellar",
			"HOMEBREW_REPOSITORY": "/opt/homebrew"
		},
		"permissions": {"allow": ["mcp__pencil"]},
		"enabledPlugins": {
			"superpowers@claude-plugins-official": true,
			"vercel@claude-plugins-official": true
		},
		"effortLevel": "high"
	}`)

	got, err := TranslateSettings(input, hostHome, vmHome)
	if err != nil {
		t.Fatalf("TranslateSettings: %v", err)
	}

	if !bytesContain(got, "/home/linuxbrew/.linuxbrew/bin") {
		t.Errorf("expected linuxbrew path, got: %s", got)
	}
	if bytesContain(got, "/opt/homebrew/bin") {
		t.Errorf("host homebrew path should not appear, got: %s", got)
	}
	if !bytesContain(got, "/home/testuser.guest/.local/bin") {
		t.Errorf("expected VM home path, got: %s", got)
	}
	if !bytesContain(got, `"superpowers@claude-plugins-official"`) {
		t.Errorf("expected enabledPlugins preserved, got: %s", got)
	}
	if !bytesContain(got, `"high"`) {
		t.Errorf("expected effortLevel preserved, got: %s", got)
	}
	if bytesContain(got, "HOMEBREW_PREFIX") {
		t.Errorf("macOS-only HOMEBREW_PREFIX should be dropped, got: %s", got)
	}
}

func TestTranslateSettingsDropsMacOSPaths(t *testing.T) {
	hostHome := "/Users/testuser"
	vmHome := "/home/testuser.guest"

	input := []byte(`{
		"env": {
			"PATH": "/Library/Apple/usr/bin:/opt/homebrew/bin:/usr/bin"
		}
	}`)

	got, err := TranslateSettings(input, hostHome, vmHome)
	if err != nil {
		t.Fatalf("TranslateSettings: %v", err)
	}

	if bytesContain(got, "/Library/") {
		t.Errorf("macOS-only /Library/ path should be dropped, got: %s", got)
	}
}

// bytesContain reports whether b contains the substring s.
func bytesContain(b []byte, s string) bool {
	return len(b) >= len(s) && strings.Contains(string(b), s)
}
