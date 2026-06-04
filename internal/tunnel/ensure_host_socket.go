package tunnel

import (
	"fmt"
	"os"
	"os/exec"
)

// ensureHostSocket verifies that hostSocket is a live Unix-domain socket on the
// host. When the socket is missing it invokes launch and re-checks, because the
// most common cause of a missing socket is that the backing daemon (the host
// gpg-agent) is not running — and gpg-agent recreates its sockets, including the
// extra-socket, when it starts.
//
// This is the fix for gpg-forward breaking on every VM entry: the reverse
// tunnel forwards to the host gpg-agent extra-socket, which exists only while
// the agent runs. Whenever gpg has not been used on the host recently the agent
// has exited and the socket is gone, so the tunnel previously failed silently.
// Launching the agent on demand makes entry self-healing.
//
// An error is returned only when the socket is still absent (or is not a
// socket) after the launch attempt, or when launch itself fails. A path that
// already exists but is not a socket is a misconfiguration the launcher cannot
// repair, so it errors immediately without launching.
func ensureHostSocket(hostSocket string, launch func() error) error {
	if fi, err := os.Stat(hostSocket); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("host socket %q is not a socket", hostSocket)
		}
		return nil
	}

	if launch != nil {
		if err := launch(); err != nil {
			return fmt.Errorf("host socket %q missing and launch failed: %w", hostSocket, err)
		}
	}

	fi, err := os.Stat(hostSocket)
	if err != nil {
		return fmt.Errorf("host socket %q not reachable after launch: %w", hostSocket, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("host socket %q is not a socket", hostSocket)
	}
	return nil
}

// launchHostGPGAgent starts the host gpg-agent if it is not already running.
// gpgconf --launch is idempotent: it is a no-op when the agent is already up,
// and otherwise starts it (which creates the standard and extra sockets the
// reverse tunnel depends on).
func launchHostGPGAgent() error {
	return exec.Command("gpgconf", "--launch", "gpg-agent").Run()
}
