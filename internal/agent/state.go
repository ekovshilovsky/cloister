package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// StateDir returns the path to the agent state directory (~/.cloister/state/).
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cloister", "state"), nil
}

// WriteContainerID persists the Docker container ID for the given profile to disk.
func WriteContainerID(stateDir, profile, containerID string) error {
	path := filepath.Join(stateDir, profile+".agent.container")
	return os.WriteFile(path, []byte(containerID), 0o600)
}

// ReadContainerID reads the stored Docker container ID for the given profile.
// Returns an error if no state file exists for the profile, indicating no running agent.
func ReadContainerID(stateDir, profile string) (string, error) {
	path := filepath.Join(stateDir, profile+".agent.container")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("no running agent for profile %q", profile)
	}
	return strings.TrimSpace(string(data)), nil
}

// RemoveContainerID deletes the container ID state file for the given profile.
func RemoveContainerID(stateDir, profile string) {
	path := filepath.Join(stateDir, profile+".agent.container")
	os.Remove(path)
}

// WriteForwardPID persists the SSH tunnel process ID for a port forward to disk.
func WriteForwardPID(stateDir, profile string, port, pid int) error {
	path := filepath.Join(stateDir, fmt.Sprintf("%s.forward.%d.pid", profile, port))
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600)
}

// ReadForwardPID reads the stored SSH tunnel process ID for the given profile and port.
func ReadForwardPID(stateDir, profile string, port int) (int, error) {
	path := filepath.Join(stateDir, fmt.Sprintf("%s.forward.%d.pid", profile, port))
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// RemoveForwardPID deletes the forward PID state file for the given profile and port.
func RemoveForwardPID(stateDir, profile string, port int) {
	path := filepath.Join(stateDir, fmt.Sprintf("%s.forward.%d.pid", profile, port))
	os.Remove(path)
}

// ListForwardPorts returns all ports with active forward PID files for the given profile.
func ListForwardPorts(stateDir, profile string) []int {
	pattern := filepath.Join(stateDir, fmt.Sprintf("%s.forward.*.pid", profile))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	prefix := profile + ".forward."
	var ports []int
	for _, m := range matches {
		base := filepath.Base(m)
		trimmed := strings.TrimPrefix(base, prefix)
		trimmed = strings.TrimSuffix(trimmed, ".pid")
		if port, err := strconv.Atoi(trimmed); err == nil {
			ports = append(ports, port)
		}
	}
	return ports
}
