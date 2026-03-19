package config

import "testing"

// TestResolveWorkspaceDir verifies that workspace directory resolution correctly
// expands tilde prefixes, falls back to ~/code when no start_dir is set, and
// returns errors for unsupported path forms such as relative paths and ~user
// syntax.
func TestResolveWorkspaceDir(t *testing.T) {
	tests := []struct {
		name      string
		startDir  string
		homeDir   string
		want      string
		wantErr   bool
		errSubstr string
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
		{
			name:      "relative path is rejected",
			startDir:  "projects/myapp",
			homeDir:   "/home/user",
			wantErr:   true,
			errSubstr: "not an absolute path",
		},
		{
			name:      "tilde-user syntax is rejected",
			startDir:  "~otheruser/work",
			homeDir:   "/home/user",
			wantErr:   true,
			errSubstr: "~user syntax which is not supported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveWorkspaceDir(tt.startDir, tt.homeDir)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ResolveWorkspaceDir(%q, %q) expected error containing %q, got nil",
						tt.startDir, tt.homeDir, tt.errSubstr)
					return
				}
				if tt.errSubstr != "" && !containsString(err.Error(), tt.errSubstr) {
					t.Errorf("ResolveWorkspaceDir(%q, %q) error = %q, want substring %q",
						tt.startDir, tt.homeDir, err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Errorf("ResolveWorkspaceDir(%q, %q) unexpected error: %v",
					tt.startDir, tt.homeDir, err)
				return
			}
			if got != tt.want {
				t.Errorf("ResolveWorkspaceDir(%q, %q) = %q, want %q",
					tt.startDir, tt.homeDir, got, tt.want)
			}
		})
	}
}

// containsString reports whether s contains the substring substr.
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}
