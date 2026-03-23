package agent

import (
	"testing"
)

func TestBuildForwardSSHArgs(t *testing.T) {
	args := buildForwardSSHArgs(3000, "/path/to/ssh.config", "lima-host")
	found := false
	for _, a := range args {
		if a == "3000:localhost:3000" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected -L 3000:localhost:3000 in args: %v", args)
	}
}

func TestBuildForwardSSHArgsContainsFlags(t *testing.T) {
	args := buildForwardSSHArgs(8080, "/config", "host")
	hasFN := false
	for _, a := range args {
		if a == "-fN" {
			hasFN = true
		}
	}
	if !hasFN {
		t.Error("forward SSH args should include -fN for background daemonization")
	}
}
