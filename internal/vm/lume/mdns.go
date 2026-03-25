package lume

import (
	"fmt"
	"os/exec"
	"time"
)

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
// via the macOS scutil utility. The default Lume password is piped to sudo
// via stdin since SSH sessions don't have a TTY for interactive password entry.
//
// This must be called after the VM is running and SSH connectivity is available.
func SetHostname(profile string, backend *Backend) error {
	hostname := Hostname(profile)
	// Pipe the default Lume password to sudo via stdin. The -S flag tells
	// sudo to read the password from stdin instead of the terminal.
	_, err := backend.SSHCommand(profile,
		fmt.Sprintf("echo 'lume' | sudo -S scutil --set LocalHostName %s && echo 'lume' | sudo -S scutil --set HostName %s",
			hostname, hostname))
	return err
}

// VerifyMDNS checks whether the VM's mDNS hostname resolves from the host by
// sending a single ICMP ping. The ping implicitly performs mDNS resolution —
// if the .local name doesn't resolve, ping fails immediately with a DNS error
// (exit code 68 on macOS). If it resolves but the host is unreachable, ping
// fails with a timeout (exit code 2). Only exit code 0 means the hostname
// resolves AND the VM is reachable.
//
// Retries every 2 seconds up to timeoutSec. Returns true on the first
// successful ping, false if the timeout is reached.
func VerifyMDNS(profile string, timeoutSec int) bool {
	hostname := MDNSName(profile)
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("ping", "-c1", "-W1", hostname).Run() == nil {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}
