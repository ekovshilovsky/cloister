package vcsbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const serviceStateVersion = 1

// ServiceState is the complete identity of one profile's standalone broker.
// Teardown authenticates the broker process by owner and the tunnel by its
// exact owner, PID, ports, and target. The token authenticates health probes.
type ServiceState struct {
	Version        int    `json:"version"`
	OwnerID        string `json:"owner_id"`
	BrokerPID      int    `json:"broker_pid"`
	TunnelPID      int    `json:"tunnel_pid"`
	HostPort       int    `json:"host_port"`
	GuestPort      int    `json:"guest_port"`
	Token          string `json:"token"`
	ConfigHash     string `json:"config_hash"`
	BuildID        string `json:"build_id"`
	Phase          string `json:"phase,omitempty"`
	TunnelTarget   string `json:"tunnel_target"`
	StatePath      string `json:"state_path"`
	ConfigPath     string `json:"config_path"`
	ReadyPath      string `json:"ready_path"`
	RepairPath     string `json:"repair_path"`
	DrainPath      string `json:"drain_path"`
	ActivityPath   string `json:"activity_path"`
	TransitionPath string `json:"transition_path"`
	SpoolDir       string `json:"spool_dir"`
	LogPath        string `json:"log_path"`
}

// StateStore holds one profile's service record and bounded cross-process lock.
type StateStore struct {
	StatePath string
	LockPath  string
	LockWait  time.Duration
}

// NewStateStore derives stable private state paths for a profile.
func NewStateStore(stateDir, profile string, lockWait time.Duration) *StateStore {
	sum := sha256.Sum256([]byte(profile))
	key := hex.EncodeToString(sum[:12])
	base := filepath.Join(stateDir, "vcs-broker-"+key)
	return &StateStore{StatePath: base + ".json", LockPath: base + ".lock", LockWait: lockWait}
}

// StateLock is a held profile service lock.
type StateLock struct {
	store *StateStore
	file  *os.File
}

// Lock waits at most LockWait. Kernel flock ownership disappears if a holder
// crashes, while the explicit timeout prevents a live stuck holder from
// hanging another command indefinitely.
func (s *StateStore) Lock(ctx context.Context) (*StateLock, error) {
	if err := os.MkdirAll(filepath.Dir(s.LockPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating VCS broker state directory: %w", err)
	}
	file, err := os.OpenFile(s.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening VCS broker lock: %w", err)
	}
	deadline := time.Now().Add(s.LockWait)
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &StateLock{store: s, file: file}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			_ = file.Close()
			return nil, fmt.Errorf("locking VCS broker state: %w", err)
		}
		if s.LockWait <= 0 || !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("timed out after %s waiting for VCS broker lock", s.LockWait)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("waiting for VCS broker lock: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Load returns an empty state when no service has been recorded.
func (l *StateLock) Load() (ServiceState, error) {
	return ReadServiceState(l.store.StatePath)
}

// Save atomically publishes a private service record.
func (l *StateLock) Save(state ServiceState) error {
	return WriteServiceState(l.store.StatePath, state)
}

// Remove deletes the current service record.
func (l *StateLock) Remove() error {
	err := os.Remove(l.store.StatePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing VCS broker state: %w", err)
	}
	return nil
}

// Close releases the profile lock.
func (l *StateLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// ReadServiceState reads an atomically published service record.
func ReadServiceState(path string) (ServiceState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ServiceState{}, nil
	}
	if err != nil {
		return ServiceState{}, fmt.Errorf("reading VCS broker state: %w", err)
	}
	var state ServiceState
	if err := json.Unmarshal(data, &state); err != nil {
		return ServiceState{}, fmt.Errorf("decoding VCS broker state: %w", err)
	}
	if state.Version != serviceStateVersion {
		return ServiceState{}, fmt.Errorf("unsupported VCS broker state version %d", state.Version)
	}
	return state, nil
}

// WriteServiceState atomically writes a private service record.
func WriteServiceState(path string, state ServiceState) error {
	state.Version = serviceStateVersion
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encoding VCS broker state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vcs-broker-state-*")
	if err != nil {
		return fmt.Errorf("creating VCS broker state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing VCS broker state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing VCS broker state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publishing VCS broker state: %w", err)
	}
	return nil
}
