package lume

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
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
// via the macOS scutil utility. Passwordless sudo must be configured in the
// VM before calling this (handled by the unattended preset's post_ssh_commands).
//
// This must be called after the VM is running and SSH connectivity is available.
func SetHostname(profile string, backend *Backend) error {
	hostname := Hostname(profile)
	_, err := backend.SSHCommand(profile,
		fmt.Sprintf("sudo scutil --set LocalHostName %s && sudo scutil --set HostName %s",
			hostname, hostname))
	return err
}

// ConnectivityCheck holds the results of the three-stage VM reachability
// verification: DNS resolution, network reachability, and SSH connectivity.
type ConnectivityCheck struct {
	// MDNSResolved is true when the .local hostname resolves to an IP via mDNS.
	MDNSResolved bool
	// ResolvedIP is the IP address returned by mDNS resolution (empty on failure).
	ResolvedIP string
	// Reachable is true when the resolved IP responds to ICMP ping.
	Reachable bool
	// SSHAvailable is true when the SSH port (22) accepts TCP connections.
	SSHAvailable bool
}

// VerifyConnectivity runs a three-stage check against the VM's mDNS hostname:
//  1. DNS resolution — does cloister-<profile>.local resolve to an IP?
//  2. Network reachability — does the resolved IP respond to ping?
//  3. SSH port — is TCP port 22 accepting connections?
//
// Each stage is independent and logged in the result. The function retries
// DNS resolution up to timeoutSec seconds before giving up. Once resolution
// succeeds, reachability and SSH are checked once.
func VerifyConnectivity(profile string, timeoutSec int) ConnectivityCheck {
	hostname := MDNSName(profile)
	result := ConnectivityCheck{}

	// Stage 1: mDNS resolution — poll until the .local name resolves or timeout.
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		ip := resolveMDNS(hostname)
		if ip != "" {
			result.MDNSResolved = true
			result.ResolvedIP = ip
			break
		}
		time.Sleep(2 * time.Second)
	}

	if !result.MDNSResolved {
		return result
	}

	// Stage 2: ICMP reachability — single ping to the resolved IP.
	result.Reachable = checkPing(result.ResolvedIP)

	// Stage 3: SSH port — TCP connect to port 22 on the resolved IP.
	result.SSHAvailable = checkTCPPort(result.ResolvedIP, 22)

	return result
}

// resolveMDNS performs a DNS lookup on the given .local hostname and returns
// the first IPv4 address, or empty string on failure. Uses net.LookupHost
// which on macOS delegates to the system resolver (including mDNS for .local).
func resolveMDNS(hostname string) string {
	addrs, err := net.LookupHost(hostname)
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		// Prefer IPv4 for SSH connections.
		if !strings.Contains(addr, ":") {
			return addr
		}
	}
	if len(addrs) > 0 {
		return addrs[0]
	}
	return ""
}

// checkPing sends a single ICMP echo request with a 1-second timeout.
// Returns true if the host responds.
func checkPing(ip string) bool {
	return exec.Command("ping", "-c1", "-W1", ip).Run() == nil
}

// checkTCPPort attempts a TCP connection to the given IP and port with a
// 2-second timeout. Returns true if the connection is accepted.
func checkTCPPort(ip string, port int) bool {
	addr := fmt.Sprintf("%s:%d", ip, port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
