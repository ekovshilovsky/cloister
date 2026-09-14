package processidentity

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestIdentityMatchesExactProcessGeneration(t *testing.T) {
	identity, err := Read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !Matches(os.Getpid(), identity) {
		t.Fatal("captured process identity did not match")
	}
	changed := identity
	changed.StartTime += "-reused"
	if Matches(os.Getpid(), changed) {
		t.Fatal("changed process start time matched")
	}
	changed = identity
	changed.Executable += ".replaced"
	if !Matches(os.Getpid(), changed) {
		t.Fatal("executable path drift overrode matching kernel start time")
	}
}

func TestObserveSeparatesDeadNotOursAndUnverifiable(t *testing.T) {
	dead := Observe(99999999, Identity{})
	if dead.State != Dead {
		t.Fatalf("dead observation = %#v", dead)
	}
	process := exec.Command("sleep", "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	identity, err := Read(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	reused := identity
	reused.StartTime += "-previous"
	if got := Observe(process.Process.Pid, reused).State; got != NotOurs {
		t.Fatalf("reused PID observation = %v, want not ours", got)
	}
	observation := Observe(process.Process.Pid, Identity{})
	if observation.State != Unverifiable || observation.Err == nil {
		t.Fatalf("missing identity observation = %#v", observation)
	}
}

func TestSignalProcessGroupRejectsAValidMemberFromAnotherGroup(t *testing.T) {
	member := exec.Command("sleep", "30")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := member.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = member.Process.Kill(); _ = member.Wait() })
	other := exec.Command("sleep", "30")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	identity, err := Read(member.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignalProcessGroup(other.Process.Pid, member.Process.Pid, identity, 0); err == nil {
		t.Fatal("member identity authorized signaling a different live process group")
	}
}
