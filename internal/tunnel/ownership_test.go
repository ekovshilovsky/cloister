package tunnel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestOwnedReverseForwardTeardownRequiresEveryIdentityField(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".cloister", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeSSH := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(fakeSSH, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	process := exec.Command(fakeSSH, "-fN", "-R", "42001:127.0.0.1:41001", "vm.test")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() {
		_ = process.Process.Kill()
	})
	claim := ReverseForwardOwner{
		OwnerID: "owner-a", PID: process.Process.Pid, HostPort: 41001,
		GuestPort: 42001, Target: "vm.test",
	}
	path := ownedReverseForwardPath(stateDir, "shared", "vcs-broker")
	if err := writeReverseForwardOwner(path, claim); err != nil {
		t.Fatal(err)
	}
	mutations := []ReverseForwardOwner{
		{OwnerID: "owner-b", PID: claim.PID, HostPort: claim.HostPort, GuestPort: claim.GuestPort, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID + 1, HostPort: claim.HostPort, GuestPort: claim.GuestPort, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID, HostPort: claim.HostPort + 1, GuestPort: claim.GuestPort, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID, HostPort: claim.HostPort, GuestPort: claim.GuestPort + 1, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID, HostPort: claim.HostPort, GuestPort: claim.GuestPort, Target: "other.test"},
	}
	for _, mutation := range mutations {
		if StopOwnedReverseForward("shared", "vcs-broker", mutation) {
			t.Fatalf("mismatched claim was accepted: %#v", mutation)
		}
		if !processAlive(claim.PID) {
			t.Fatal("mismatched claim killed the tunnel")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("mismatched claim removed ownership record: %v", err)
		}
	}
	if !StopOwnedReverseForward("shared", "vcs-broker", claim) {
		t.Fatal("exact owner could not stop its tunnel")
	}
	<-done
	if processAlive(claim.PID) {
		t.Fatal("exact owner left the tunnel alive")
	}
}

func TestOwnedReverseForwardDoesNotKillReusedUnrelatedPID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".cloister", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	process := exec.Command("sleep", "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() {
		_ = process.Process.Kill()
	})
	claim := ReverseForwardOwner{OwnerID: "stale", PID: process.Process.Pid, HostPort: 41001, GuestPort: 42001, Target: "vm.test"}
	path := ownedReverseForwardPath(stateDir, "stale", "vcs-broker")
	if err := writeReverseForwardOwner(path, claim); err != nil {
		t.Fatal(err)
	}
	if !StopOwnedReverseForward("stale", "vcs-broker", claim) {
		t.Fatal("exact stale record was not consumed")
	}
	select {
	case <-done:
		t.Fatal("reused PID belonging to an unrelated process was killed")
	case <-time.After(100 * time.Millisecond):
	}
	legacyPath := filepath.Join(stateDir, "tunnel-vcs-broker-stale.pid")
	if err := os.WriteFile(legacyPath, []byte(strconv.Itoa(process.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if legacyReverseForwardMatches(process.Process.Pid, 42001, "vm.test") {
		t.Fatal("legacy stale PID was mistaken for the expected ssh tunnel")
	}
	if err := os.WriteFile(legacyPath, []byte(strconv.Itoa(process.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	StopAll("stale")
	select {
	case <-done:
		t.Fatal("lifecycle cleanup killed an unrelated process from a stale VCS PID file")
	case <-time.After(100 * time.Millisecond):
	}
	if err := writeReverseForwardOwner(path, claim); err != nil {
		t.Fatal(err)
	}
	StopAll("stale")
	select {
	case <-done:
		t.Fatal("lifecycle cleanup killed an unrelated process from a stale owned-tunnel record")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("generic tunnel cleanup removed broker-owned claim: %v", err)
	}
}
