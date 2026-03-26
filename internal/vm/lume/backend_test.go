package lume_test

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/ekovshilovsky/cloister/internal/vm/lume"
)

// Compile-time assertions that *Backend satisfies vm.Backend, vm.NATNetworker,
// and vm.GoldenImageManager. If any required method is missing or has an
// incorrect signature, these assignments will fail at compile time with a clear
// diagnostic.
var _ vm.Backend = (*lume.Backend)(nil)
var _ vm.NATNetworker = (*lume.Backend)(nil)
var _ vm.GoldenImageManager = (*lume.Backend)(nil)

// TestVMName verifies that VMName correctly prepends the cloister prefix to a
// profile name, producing the Lume VM name used internally.
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
		got := lume.VMName(tc.profile)
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
		{"lume-default", ""},
		{"default", ""},
		{"", ""},
		// A string equal to the bare prefix with no profile segment following it.
		{"cloister-", ""},
	}

	for _, tc := range cases {
		got := lume.ProfileFromVMName(tc.vmName)
		if got != tc.want {
			t.Errorf("ProfileFromVMName(%q) = %q, want %q", tc.vmName, got, tc.want)
		}
	}
}
