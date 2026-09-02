package vm

import (
	"fmt"
	"io"
)

// Backend is the abstraction layer for VM lifecycle management. Implementations
// of this interface wrap a specific hypervisor CLI (e.g. Colima, Lume) so that
// higher-level cloister logic can remain decoupled from any single tool.
//
// Each method accepts a profile name that identifies a cloister-managed VM.
// Implementations are responsible for translating the profile name into the
// backend-specific instance identifier (e.g. by prepending a vendor prefix).
type Backend interface {
	// Start creates or resumes the VM using the validated workspace mode,
	// supplemental mounts, resources, and output policy in spec.
	Start(profile string, spec StartSpec) error

	// Stop gracefully shuts down the running VM for the given profile. It must
	// be idempotent: stopping an already-stopped VM must not return an error.
	// When verbose is true, hypervisor output is forwarded to stderr.
	Stop(profile string, verbose bool) error

	// Delete permanently destroys the VM for the given profile and releases all
	// associated resources. When verbose is true, hypervisor output is forwarded
	// to stderr.
	Delete(profile string, verbose bool) error

	// Exists reports whether a VM instance for the given profile is registered
	// with the hypervisor, regardless of whether it is currently running.
	Exists(profile string) bool

	// IsRunning reports whether the VM for the given profile is in the running
	// state. It returns false if the VM does not exist or cannot be queried.
	IsRunning(profile string) bool

	// List returns status metadata for all VM instances managed by this backend
	// that were created by cloister (i.e. carrying the cloister namespace prefix).
	// When verbose is true, hypervisor output is forwarded to stderr.
	List(verbose bool) ([]VMStatus, error)

	// SSH attaches an interactive terminal session to the VM for the given
	// profile. stdin, stdout, and stderr are connected directly to the
	// hypervisor process so the caller receives a fully functional shell.
	SSH(profile string) error

	// SSHCommand runs a non-interactive command inside the VM and returns what
	// it printed. The command is executed via a login shell so that the guest
	// environment (PATH, profile scripts) is initialised.
	//
	// Implementations differ in what the returned string carries. Colima
	// returns stdout and stderr together; Lume returns stdout alone, so that a
	// parsed result cannot be corrupted by a warning, and puts stderr in the
	// error instead. A caller that wants the guest's account of a failure has
	// to read the error as well as the output.
	SSHCommand(profile string, command string) (string, error)

	// SSHInteractive runs a command inside the VM with stdin/stdout/stderr
	// connected to the current terminal. This is suitable for streaming commands
	// (e.g. following a log) that require direct terminal access.
	SSHInteractive(profile string, command string) error

	// SSHScript pipes a multi-line shell script into the VM via stdin, avoiding
	// the quoting complications that arise when embedding complex scripts in a
	// single command argument. Implementations may stream the script output to
	// the terminal for live provisioning progress.
	SSHScript(profile string, script string) (string, error)

	// SSHScriptTo is SSHScript with the guest output directed somewhere other
	// than the terminal. Provisioning sends it to a run log so the console can
	// report progress rather than reproduce hundreds of lines of package
	// manager output; SSHScript is this called with the terminal.
	SSHScriptTo(profile string, script string, out io.Writer) (string, error)

	// SSHCapture runs a stdin-piped script like SSHScript but never streams the
	// guest output to the terminal. It is used for control and value-resolution
	// commands (for example resolving $HOME behind sentinels) whose output must
	// not leak into the user's session or corrupt a parsed result.
	SSHCapture(profile string, script string) (string, error)

	// SSHConfig returns the SSH connection parameters for the given profile.
	// Callers may use the returned SSHAccess values to construct an ssh(1)
	// invocation or a programmatic SSH client connection.
	SSHConfig(profile string) SSHAccess

	// VMName returns the hypervisor-level instance name for the given profile.
	// This is distinct from the cloister profile name and is typically prefixed
	// with a backend-specific namespace string (e.g. "cloister-<profile>").
	VMName(profile string) string

	// ProfileFromVMName extracts the cloister profile name from a
	// hypervisor-level instance name. It returns an empty string when the
	// instance name was not created by cloister.
	ProfileFromVMName(vmName string) string
}

// VMStatus captures the subset of VM instance metadata that cloister needs for
// display and lifecycle decisions. The JSON tags match the field names emitted
// by `colima list --json` and are the canonical representation shared across
// all Backend implementations.
type VMStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	CPUs   int    `json:"cpus"`
	Memory int    `json:"memory"`
	Disk   int    `json:"disk"`
	Arch   string `json:"arch"`
}

// SSHAccess holds the connection parameters required to open an SSH session to
// a cloister-managed VM. Callers construct an ssh(1) command or a programmatic
// SSH client from these values.
type SSHAccess struct {
	// ConfigFile is the absolute path to the SSH client configuration file
	// generated by the hypervisor (e.g. the Lima-generated ssh.config file).
	// Pass this to `ssh -F` to reach the VM without specifying individual flags.
	ConfigFile string

	// HostAlias is the Host entry within ConfigFile that identifies this VM
	// (e.g. "lima-colima-cloister-work").
	HostAlias string

	// Host is the IP address or hostname used to reach the VM directly, for
	// backends that expose a routable address rather than an SSH config alias.
	Host string

	// User is the username with which to authenticate inside the VM.
	User string

	// KeyFile is the absolute path to the private key for key-based
	// authentication. May be empty when the hypervisor manages key injection
	// through its own configuration file.
	KeyFile string
}

// WorkspaceProvider identifies how a workspace reaches the guest. The value is
// explicit so backend code cannot infer privilege or purpose from mount order.
type WorkspaceProvider uint8

const (
	VirtiofsWorkspace WorkspaceProvider = iota
	BrokerWorkspace
	// WorkspaceBroker represents a multi-project collection whose routing
	// root is not itself synchronized.
	WorkspaceBroker
	NoWorkspace
)

// IsBroker reports whether the provider uses synchronized guest copies.
func (p WorkspaceProvider) IsBroker() bool {
	return p == BrokerWorkspace || p == WorkspaceBroker
}

// StartSpec describes a complete backend start without mixing the workspace
// transport with fixed supplemental host shares.
type StartSpec struct {
	CPUs               int
	MemoryGB           int
	DiskGB             int
	RootDiskGB         int
	SupplementalMounts []Mount
	WorkspaceMount     *Mount
	MountInotify       bool
	WorkspaceProvider  WorkspaceProvider
	Verbose            bool
}

// Mounts returns the backend-visible mounts after validating the workspace
// provider contract.
func (s StartSpec) Mounts() ([]Mount, error) {
	mounts := make([]Mount, 0, len(s.SupplementalMounts)+1)
	switch s.WorkspaceProvider {
	case VirtiofsWorkspace:
		if s.WorkspaceMount == nil {
			return nil, fmt.Errorf("virtiofs workspace provider requires a workspace mount")
		}
		mounts = append(mounts, *s.WorkspaceMount)
	case BrokerWorkspace, WorkspaceBroker, NoWorkspace:
		if s.WorkspaceMount != nil {
			return nil, fmt.Errorf("workspace provider %d cannot include a VM workspace mount", s.WorkspaceProvider)
		}
	default:
		return nil, fmt.Errorf("unknown workspace provider %d", s.WorkspaceProvider)
	}
	mounts = append(mounts, s.SupplementalMounts...)
	return mounts, nil
}

// MockBackend is a test-only implementation of Backend that records method
// invocations and returns pre-configured canned responses. It is intended for
// use in unit tests that need to drive cloister logic without spawning real VMs.
type MockBackend struct {
	// StartCalls records the profile argument for each Start invocation, in
	// the order the calls were received.
	StartCalls []string

	// StartSpecs records the full value object passed to each Start invocation.
	StartSpecs []StartSpec

	// StopCalls records the profile argument for each Stop invocation.
	StopCalls []string

	// SSHCommandCalls records the profile and command argument for each
	// SSHCommand invocation.
	SSHCommandCalls []struct{ Profile, Command string }

	// SSHCommandOut is the output string returned by all SSHCommand calls.
	SSHCommandOut string

	// SSHCommandErr is the error returned by all SSHCommand calls.
	SSHCommandErr error

	// SSHInteractiveCalls records terminal-bound commands.
	SSHInteractiveCalls []struct{ Profile, Command string }

	// SSHInteractiveErr is returned by SSHInteractive.
	SSHInteractiveErr error

	// SSHScriptCalls records the profile and script argument for each
	// SSHScript invocation.
	SSHScriptCalls []struct{ Profile, Script string }

	// SSHScriptOut is the output string returned by all SSHScript calls.
	SSHScriptOut string

	// SSHScriptErr is the error returned by all SSHScript calls.
	SSHScriptErr error

	// RunningProfiles maps profile names to their simulated running state.
	// IsRunning returns the value for the queried profile (false when absent).
	RunningProfiles map[string]bool

	// SSHAccessVal is the SSHAccess value returned by all SSHConfig calls.
	SSHAccessVal SSHAccess
}

// Start records the call and returns nil.
func (m *MockBackend) Start(profile string, spec StartSpec) error {
	m.StartCalls = append(m.StartCalls, profile)
	m.StartSpecs = append(m.StartSpecs, spec)
	return nil
}

// Stop records the call and returns nil.
func (m *MockBackend) Stop(profile string, verbose bool) error {
	m.StopCalls = append(m.StopCalls, profile)
	return nil
}

// Delete records the call and returns nil. A separate DeleteCalls field is not
// provided because Delete is rarely the primary observable in unit tests; callers
// can instrument a custom mock when fine-grained Delete tracking is required.
func (m *MockBackend) Delete(profile string, verbose bool) error {
	return nil
}

// Exists always returns true, indicating the VM is registered with the backend.
// Override by embedding MockBackend in a custom struct that shadows this method.
func (m *MockBackend) Exists(profile string) bool {
	return true
}

// IsRunning returns the value recorded in RunningProfiles for the given profile,
// or false if the profile is not present in the map.
func (m *MockBackend) IsRunning(profile string) bool {
	if m.RunningProfiles == nil {
		return false
	}
	return m.RunningProfiles[profile]
}

// List returns an empty slice and no error. Tests that need to exercise listing
// behavior should populate a custom mock with the desired VMStatus entries.
func (m *MockBackend) List(verbose bool) ([]VMStatus, error) {
	return nil, nil
}

// SSH is a no-op that returns nil. Interactive shell testing requires a real
// terminal and is therefore out of scope for unit-level mocks.
func (m *MockBackend) SSH(profile string) error {
	return nil
}

// SSHCommand records the invocation and returns SSHCommandOut and SSHCommandErr.
func (m *MockBackend) SSHCommand(profile string, command string) (string, error) {
	m.SSHCommandCalls = append(m.SSHCommandCalls, struct{ Profile, Command string }{profile, command})
	return m.SSHCommandOut, m.SSHCommandErr
}

// SSHInteractive records the terminal-bound command without opening a terminal.
func (m *MockBackend) SSHInteractive(profile string, command string) error {
	m.SSHInteractiveCalls = append(m.SSHInteractiveCalls, struct{ Profile, Command string }{profile, command})
	return m.SSHInteractiveErr
}

// SSHScript records the invocation and returns SSHScriptOut and SSHScriptErr.
func (m *MockBackend) SSHScriptTo(profile string, script string, out io.Writer) (string, error) {
	if out != nil && m.SSHScriptOut != "" {
		// Recording backends still exercise the caller's sink, so a test can
		// assert on what provisioning would have written to the run log.
		if _, err := io.WriteString(out, m.SSHScriptOut); err != nil {
			return "", err
		}
	}
	return m.SSHScript(profile, script)
}

func (m *MockBackend) SSHScript(profile string, script string) (string, error) {
	m.SSHScriptCalls = append(m.SSHScriptCalls, struct{ Profile, Script string }{profile, script})
	return m.SSHScriptOut, m.SSHScriptErr
}

// SSHCapture mirrors SSHScript for tests: it records the invocation on the same
// SSHScriptCalls slice and returns SSHScriptOut and SSHScriptErr so a caller
// switching between the two behaves identically under test.
func (m *MockBackend) SSHCapture(profile string, script string) (string, error) {
	m.SSHScriptCalls = append(m.SSHScriptCalls, struct{ Profile, Script string }{profile, script})
	return m.SSHScriptOut, m.SSHScriptErr
}

// SSHConfig returns SSHAccessVal, the pre-configured SSH connection parameters.
func (m *MockBackend) SSHConfig(profile string) SSHAccess {
	return m.SSHAccessVal
}

// VMName returns a synthetic VM name by prepending the "mock-" namespace prefix
// to the profile, mirroring the naming convention used by real backends.
func (m *MockBackend) VMName(profile string) string {
	return "mock-" + profile
}

// ProfileFromVMName strips the "mock-" prefix and returns the profile segment.
// It returns an empty string when the name does not carry the mock prefix.
func (m *MockBackend) ProfileFromVMName(vmName string) string {
	const prefix = "mock-"
	if len(vmName) <= len(prefix) || vmName[:len(prefix)] != prefix {
		return ""
	}
	return vmName[len(prefix):]
}
