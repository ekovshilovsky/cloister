package cmd

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
)

// TestShouldAutoAddTunnel verifies the logic that determines whether the ollama
// tunnel name should be automatically appended to an explicit tunnel allowlist
// when the ollama stack is added to a profile.
func TestShouldAutoAddTunnel(t *testing.T) {
	tests := []struct {
		name   string
		stack  string
		policy config.ResourcePolicy
		want   bool
	}{
		{
			name:   "ollama with explicit list missing ollama",
			stack:  "ollama",
			policy: config.ResourcePolicy{IsSet: true, Names: []string{"clipboard"}},
			want:   true,
		},
		{
			name:   "ollama with explicit list already has ollama",
			stack:  "ollama",
			policy: config.ResourcePolicy{IsSet: true, Names: []string{"ollama"}},
			want:   false,
		},
		{
			name:   "ollama with auto policy",
			stack:  "ollama",
			policy: config.ResourcePolicy{IsSet: true, Mode: "auto"},
			want:   false,
		},
		{
			name:   "ollama with unset policy",
			stack:  "ollama",
			policy: config.ResourcePolicy{},
			want:   false,
		},
		{
			name:   "non-ollama stack",
			stack:  "web",
			policy: config.ResourcePolicy{IsSet: true, Names: []string{"clipboard"}},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldAutoAddTunnel(tt.stack, tt.policy)
			if got != tt.want {
				t.Errorf("shouldAutoAddTunnel(%q, %+v) = %v, want %v", tt.stack, tt.policy, got, tt.want)
			}
		})
	}
}
