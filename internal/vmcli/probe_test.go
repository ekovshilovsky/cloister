package vmcli

import (
	"net"
	"testing"
	"time"
)

func TestProbeTCPAvailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	if !ProbeTCP("127.0.0.1", port, 500*time.Millisecond) {
		t.Error("ProbeTCP should return true for listening port")
	}
}

func TestProbeTCPUnavailable(t *testing.T) {
	if ProbeTCP("127.0.0.1", 59999, 100*time.Millisecond) {
		t.Error("ProbeTCP should return false for non-listening port")
	}
}

func TestProbeTCPTimeout(t *testing.T) {
	start := time.Now()
	ProbeTCP("127.0.0.1", 59999, 100*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Errorf("ProbeTCP should respect timeout, took %v", elapsed)
	}
}
