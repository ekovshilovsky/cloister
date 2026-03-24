package colima

import "strings"

// vmPrefix is prepended to every profile name to form the Colima instance
// name. This namespace prevents cloister-managed VMs from colliding with
// any Colima instances the user may have created independently.
const vmPrefix = "cloister-"

// VMName returns the Colima instance name for the given cloister profile.
// All Colima operations performed by cloister use this name so that managed
// VMs are clearly identified and easily filtered.
func VMName(profile string) string {
	return vmPrefix + profile
}

// ProfileFromVMName extracts the cloister profile name from a Colima instance
// name. It returns an empty string when the instance name does not carry the
// cloister prefix, which indicates the VM was not created by cloister.
func ProfileFromVMName(vmName string) string {
	if !strings.HasPrefix(vmName, vmPrefix) {
		return ""
	}
	profile := strings.TrimPrefix(vmName, vmPrefix)
	// Reject the degenerate case where the name equals the bare prefix with no
	// profile segment following it (e.g. "cloister-").
	if profile == "" {
		return ""
	}
	return profile
}
