package linux

import (
	"reflect"
	"testing"
)

func TestExpandStackDependencies(t *testing.T) {
	tests := []struct {
		name      string
		requested []string
		want      []string
	}{
		{
			name:      "web pulls in art",
			requested: []string{"web"},
			want:      []string{"web", "art"},
		},
		{
			name:      "web first then art is deduplicated",
			requested: []string{"web", "art"},
			want:      []string{"web", "art"},
		},
		{
			name:      "art alone does not imply web",
			requested: []string{"art"},
			want:      []string{"art"},
		},
		{
			name:      "art before web preserves user order then dedupes",
			requested: []string{"art", "web"},
			want:      []string{"art", "web"},
		},
		{
			name:      "stacks without deps pass through unchanged",
			requested: []string{"python", "go", "data"},
			want:      []string{"python", "go", "data"},
		},
		{
			name:      "web mixed with unrelated stacks inserts art right after web",
			requested: []string{"python", "web", "data"},
			want:      []string{"python", "web", "art", "data"},
		},
		{
			name:      "empty input returns empty slice",
			requested: []string{},
			want:      []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := expandStackDependencies(tc.requested)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("expandStackDependencies(%v) = %v, want %v", tc.requested, got, tc.want)
			}
		})
	}
}
