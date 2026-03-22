package vmcli

import (
	"fmt"
	"net"
	"time"
)

// ProbeTCP dials host:port with the given timeout and returns true when the
// connection is accepted. Used to check tunnel availability inside the VM.
func ProbeTCP(host string, port int, timeout time.Duration) bool {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
