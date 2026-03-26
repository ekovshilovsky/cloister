package vm_test

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/vm"
)

func TestResolveBackendName(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"", "colima", false},
		{"colima", "colima", false},
		{"lume", "lume", false},
		{"COLIMA", "", true},
		{"unknown", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := vm.ResolveBackendName(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
