package agent

import (
	"testing"
)

func TestWriteAndReadContainerID(t *testing.T) {
	dir := t.TempDir()
	if err := WriteContainerID(dir, "openclaw", "abc123def"); err != nil {
		t.Fatalf("write: %v", err)
	}
	id, err := ReadContainerID(dir, "openclaw")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if id != "abc123def" {
		t.Errorf("container ID = %q, want abc123def", id)
	}
}

func TestReadContainerIDMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadContainerID(dir, "nonexistent")
	if err == nil {
		t.Error("expected error for missing container ID")
	}
}

func TestRemoveContainerID(t *testing.T) {
	dir := t.TempDir()
	WriteContainerID(dir, "openclaw", "abc123")
	RemoveContainerID(dir, "openclaw")
	_, err := ReadContainerID(dir, "openclaw")
	if err == nil {
		t.Error("expected error after removal")
	}
}

func TestWriteAndReadForwardPID(t *testing.T) {
	dir := t.TempDir()
	if err := WriteForwardPID(dir, "openclaw", 3000, 12345); err != nil {
		t.Fatalf("write: %v", err)
	}
	pid, err := ReadForwardPID(dir, "openclaw", 3000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if pid != 12345 {
		t.Errorf("PID = %d, want 12345", pid)
	}
}

func TestRemoveForwardPID(t *testing.T) {
	dir := t.TempDir()
	WriteForwardPID(dir, "openclaw", 3000, 12345)
	RemoveForwardPID(dir, "openclaw", 3000)
	_, err := ReadForwardPID(dir, "openclaw", 3000)
	if err == nil {
		t.Error("expected error after removal")
	}
}

func TestListForwardPorts(t *testing.T) {
	dir := t.TempDir()
	WriteForwardPID(dir, "openclaw", 3000, 111)
	WriteForwardPID(dir, "openclaw", 8080, 222)
	WriteForwardPID(dir, "other", 3000, 333) // different profile

	ports := ListForwardPorts(dir, "openclaw")
	if len(ports) != 2 {
		t.Fatalf("expected 2 ports, got %d", len(ports))
	}
}
