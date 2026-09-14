package tunnel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cloister.io/internal/processidentity"
)

func TestOwnedReverseForwardTeardownRequiresEveryIdentityField(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".cloister", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	process := exec.Command("/usr/bin/ssh", "-N", "-R", "42001:127.0.0.1:41001", "-o", "ProxyCommand=sleep 30", "vm.test")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() {
		_ = process.Process.Kill()
	})
	identity, err := processidentity.Read(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	claim := ReverseForwardOwner{
		OwnerID: "owner-a", PID: process.Process.Pid, HostPort: 41001,
		ProcessIdentity: identity, GuestPort: 42001, Target: "vm.test",
	}
	path := ownedReverseForwardPath(stateDir, "shared", "vcs-broker")
	if err := writeReverseForwardOwner(path, claim); err != nil {
		t.Fatal(err)
	}
	mutations := []ReverseForwardOwner{
		{OwnerID: "owner-b", PID: claim.PID, ProcessIdentity: claim.ProcessIdentity, HostPort: claim.HostPort, GuestPort: claim.GuestPort, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID + 1, ProcessIdentity: claim.ProcessIdentity, HostPort: claim.HostPort, GuestPort: claim.GuestPort, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID, ProcessIdentity: processidentity.Identity{StartTime: "wrong", Executable: claim.ProcessIdentity.Executable}, HostPort: claim.HostPort, GuestPort: claim.GuestPort, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID, ProcessIdentity: processidentity.Identity{StartTime: claim.ProcessIdentity.StartTime, Executable: "/wrong/ssh"}, HostPort: claim.HostPort, GuestPort: claim.GuestPort, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID, ProcessIdentity: claim.ProcessIdentity, HostPort: claim.HostPort + 1, GuestPort: claim.GuestPort, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID, ProcessIdentity: claim.ProcessIdentity, HostPort: claim.HostPort, GuestPort: claim.GuestPort + 1, Target: claim.Target},
		{OwnerID: claim.OwnerID, PID: claim.PID, ProcessIdentity: claim.ProcessIdentity, HostPort: claim.HostPort, GuestPort: claim.GuestPort, Target: "other.test"},
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

func TestOwnedReverseForwardRejectsUnrelatedShellWithMatchingCommandText(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".cloister", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	process := exec.Command("/bin/sh", "-c", "sleep 30 & wait", "ssh", "-R", "42001:127.0.0.1:41001", "vm.test")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() { _ = process.Process.Kill() })
	identity, err := processidentity.Read(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	identity.Executable = "/usr/bin/ssh"
	claim := ReverseForwardOwner{
		OwnerID: "owner-a", PID: process.Process.Pid, ProcessIdentity: identity,
		HostPort: 41001, GuestPort: 42001, Target: "vm.test",
	}
	path := ownedReverseForwardPath(stateDir, "attack", "vcs-broker")
	if err := writeReverseForwardOwner(path, claim); err != nil {
		t.Fatal(err)
	}
	if !StopOwnedReverseForward("attack", "vcs-broker", claim) {
		t.Fatal("exact stale claim was not consumed")
	}
	select {
	case <-done:
		t.Fatal("unrelated shell with matching SSH text was killed")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStopAllRejectsUnrelatedShellWithForgedSSHText(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".cloister", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	process := exec.Command("/bin/sh", "-c", "sleep 30 & wait", "ssh", "-R", "42001:127.0.0.1:41001", "vm.test")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() { _ = process.Process.Kill() })
	identity, err := processidentity.Read(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	identity.Executable = "/usr/bin/ssh"
	record, err := json.Marshal(tunnelProcessRecord{PID: process.Process.Pid, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, "tunnel-clipboard-example.pid")
	if err := os.WriteFile(path, record, 0o600); err != nil {
		t.Fatal(err)
	}
	StopAll("example")
	select {
	case <-done:
		t.Fatal("StopAll killed an unrelated shell carrying SSH command text")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("StopAll retained consumed forged record: %v", err)
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
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("generic tunnel cleanup removed legacy broker-owned PID record: %v", err)
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
