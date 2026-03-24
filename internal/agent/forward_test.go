package agent

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/vm"
)

func TestBuildForwardSSHArgsWithConfigFile(t *testing.T) {
	access := vm.SSHAccess{
		ConfigFile: "/path/to/ssh.config",
		HostAlias:  "lima-host",
	}
	args := buildForwardSSHArgs(3000, access)
	found := false
	for _, a := range args {
		if a == "3000:localhost:3000" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected -L 3000:localhost:3000 in args: %v", args)
	}

	// Verify ConfigFile-based args include -F and the host alias.
	hasF := false
	hasAlias := false
	for i, a := range args {
		if a == "-F" && i+1 < len(args) && args[i+1] == "/path/to/ssh.config" {
			hasF = true
		}
		if a == "lima-host" {
			hasAlias = true
		}
	}
	if !hasF {
		t.Error("ConfigFile-based forward should include -F with the config path")
	}
	if !hasAlias {
		t.Error("ConfigFile-based forward should include the HostAlias as the target")
	}
}

func TestBuildForwardSSHArgsWithDirectHost(t *testing.T) {
	access := vm.SSHAccess{
		Host:    "192.168.64.5",
		User:    "admin",
		KeyFile: "/path/to/key",
	}
	args := buildForwardSSHArgs(8080, access)

	found := false
	for _, a := range args {
		if a == "8080:localhost:8080" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected -L 8080:localhost:8080 in args: %v", args)
	}

	// Verify direct-host args include -i, StrictHostKeyChecking, and user@host.
	hasKey := false
	hasTarget := false
	hasStrict := false
	for i, a := range args {
		if a == "-i" && i+1 < len(args) && args[i+1] == "/path/to/key" {
			hasKey = true
		}
		if a == "admin@192.168.64.5" {
			hasTarget = true
		}
		if a == "StrictHostKeyChecking=no" {
			hasStrict = true
		}
	}
	if !hasKey {
		t.Error("direct-host forward should include -i with the key file path")
	}
	if !hasTarget {
		t.Error("direct-host forward should include user@host as the target")
	}
	if !hasStrict {
		t.Error("direct-host forward should disable StrictHostKeyChecking")
	}
}

func TestBuildForwardSSHArgsContainsFlags(t *testing.T) {
	access := vm.SSHAccess{
		ConfigFile: "/config",
		HostAlias:  "host",
	}
	args := buildForwardSSHArgs(8080, access)
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
