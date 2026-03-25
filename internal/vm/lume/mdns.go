package lume

import "fmt"

// Hostname returns the mDNS hostname for the given cloister profile. The
// hostname matches the Lume VM name so that it is consistent with the instance
// identifier used by all other backend operations.
func Hostname(profile string) string {
	return VMName(profile)
}

// MDNSName returns the fully-qualified mDNS name for the given cloister
// profile in the ".local" domain, which is how the VM is reachable on the
// local network without manual DNS configuration.
func MDNSName(profile string) string {
	return Hostname(profile) + ".local"
}

// SetHostname configures the VM's hostname for mDNS advertisement. It sets
// both LocalHostName (used by Bonjour/mDNS) and HostName (the UNIX hostname)
// via the macOS scutil utility. This must be called after the VM is running
// and SSH connectivity is available.
func SetHostname(profile string, backend *Backend) error {
	hostname := Hostname(profile)
	_, err := backend.SSHCommand(profile,
		fmt.Sprintf("sudo scutil --set LocalHostName %s && sudo scutil --set HostName %s",
			hostname, hostname))
	return err
}
