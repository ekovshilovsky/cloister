package config

import "testing"

// TestResolveWorkspaceDir verifies that workspace directory resolution correctly
// expands tilde prefixes and falls back to ~/code when no start_dir is set.
func TestResolveWorkspaceDir(t *testing.T) {
	tests := []struct {
		name     string
		startDir string
		homeDir  string
		want     string
	}{
		{
			name:     "empty defaults to ~/code",
			startDir: "",
			homeDir:  "/home/user",
			want:     "/home/user/code",
		},
		{
			name:     "tilde expansion",
			startDir: "~/Projects/app",
			homeDir:  "/home/user",
			want:     "/home/user/Projects/app",
		},
		{
			name:     "absolute path",
			startDir: "/opt/work",
			homeDir:  "/home/user",
			want:     "/opt/work",
		},
		{
			name:     "bare tilde",
			startDir: "~",
			homeDir:  "/home/user",
			want:     "/home/user",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveWorkspaceDir(tt.startDir, tt.homeDir)
			if got != tt.want {
				t.Errorf("ResolveWorkspaceDir(%q, %q) = %q, want %q",
					tt.startDir, tt.homeDir, got, tt.want)
			}
		})
	}
}
