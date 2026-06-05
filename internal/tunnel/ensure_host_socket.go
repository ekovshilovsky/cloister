package tunnel

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

// hostSocketDialTimeout bounds the liveness probe so a wedged agent cannot hang
// VM entry. A live local gpg-agent accepts the connection immediately; this is
// generous headroom for a loaded machine.
const hostSocketDialTimeout = 2 * time.Second

// ensureHostSocket verifies that hostSocket is a *live* Unix-domain socket on
// the host — one a process is actually listening on — and launches the backing
// daemon (the host gpg-agent) when it is not, because gpg-agent recreates its
// sockets, including the extra-socket, when it starts.
//
// This guards gpg-forward, whose reverse tunnel forwards the VM's socket to the
// host gpg-agent extra-socket. Two distinct failure modes break it:
//
//  1. The socket file is absent because the agent exited when idle. Stat alone
//     catches this.
//  2. The socket file is present but DEAD: gpg-agent's sockets live in
//     ~/.gnupg, so the inode survives the agent exiting and survives reboots.
//     A stat-only check is fooled into reporting success, the tunnel forwards
//     to a socket nothing is listening on, and the VM reports "End of file" /
//     "no gpg-agent running" with no obvious cause. This is why an
//     existence-only check still let signing break after a host restart.
//
// Probing with a real dial collapses both modes into one: a missing or dead
// socket fails to connect, so we launch the agent and re-probe. An error is
// returned only when the socket is still not listening after the launch
// attempt, or when launch itself fails. A path that exists but is not a socket
// is a misconfiguration the launcher cannot repair, so it errors immediately
// without launching.
func ensureHostSocket(hostSocket string, launch func() error) error {
	if err := hostSocketUnusable(hostSocket); err != nil {
		return err
	}
	if hostSocketListening(hostSocket) {
		return nil
	}

	if launch != nil {
		if err := launch(); err != nil {
			return fmt.Errorf("host socket %q not listening and launch failed: %w", hostSocket, err)
		}
	}

	if err := hostSocketUnusable(hostSocket); err != nil {
		return err
	}
	if hostSocketListening(hostSocket) {
		return nil
	}
	return fmt.Errorf("host socket %q still not listening after launch", hostSocket)
}

// hostSocketUnusable reports a non-nil error when the path exists but is not a
// Unix socket — a misconfiguration that launching the agent cannot repair, so
// callers must fail fast rather than relaunch. A missing path is not an error
// here: the launcher is expected to create it.
func hostSocketUnusable(hostSocket string) error {
	fi, err := os.Stat(hostSocket)
	if err != nil {
		return nil
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("host socket %q is not a socket", hostSocket)
	}
	return nil
}

// hostSocketListening reports whether a process is actually accepting
// connections on the socket. A stale socket file left behind by an exited agent
// still passes os.Stat but refuses the dial, which is exactly the case an
// existence check misses.
func hostSocketListening(hostSocket string) bool {
	conn, err := net.DialTimeout("unix", hostSocket, hostSocketDialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// launchHostGPGAgent starts the host gpg-agent if it is not already running.
// gpgconf --launch is idempotent: it is a no-op when the agent is already up,
// and otherwise starts it (which creates the standard and extra sockets the
// reverse tunnel depends on).
func launchHostGPGAgent() error {
	return exec.Command("gpgconf", "--launch", "gpg-agent").Run()
}
