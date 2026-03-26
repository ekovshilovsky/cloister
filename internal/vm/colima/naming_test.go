package colima_test

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/vm/colima"
)

// TestVMName verifies that VMName correctly prepends the cloister prefix to a
// profile name, producing the Colima instance name used internally.
func TestVMName(t *testing.T) {
	cases := []struct {
		profile string
		want    string
	}{
		{"dev", "cloister-dev"},
		{"work", "cloister-work"},
		{"personal", "cloister-personal"},
		{"", "cloister-"},
	}

	for _, tc := range cases {
		got := colima.VMName(tc.profile)
		if got != tc.want {
			t.Errorf("VMName(%q) = %q, want %q", tc.profile, got, tc.want)
		}
	}
}

// TestProfileFromVMName verifies that ProfileFromVMName correctly strips the
// cloister prefix from a VM name, and returns an empty string for VM names
// that were not created by cloister.
func TestProfileFromVMName(t *testing.T) {
	cases := []struct {
		vmName string
		want   string
	}{
		{"cloister-dev", "dev"},
		{"cloister-work", "work"},
		{"cloister-personal", "personal"},
		// VM names that do not carry the cloister prefix must return empty string.
		{"other-vm", ""},
		{"colima-default", ""},
		{"default", ""},
		{"", ""},
		// A string equal to the bare prefix with no profile segment following it.
		{"cloister-", ""},
	}

	for _, tc := range cases {
		got := colima.ProfileFromVMName(tc.vmName)
		if got != tc.want {
			t.Errorf("ProfileFromVMName(%q) = %q, want %q", tc.vmName, got, tc.want)
		}
	}
}
