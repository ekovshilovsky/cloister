// Proprietary and confidential. All rights reserved.

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"cloister.io/internal/broker"
	brokerignore "cloister.io/internal/broker/ignore"
	"cloister.io/internal/vm"
)

// ErrRecoveryDeclined tells the coordinator to preserve the original backend
// start error after a stale-lock recovery was not authorized.
var ErrRecoveryDeclined = errors.New("stale-lock recovery declined")

// StartRequest contains command-level inputs. The coordinator alone converts a
// host workspace path into a VM mount.
type StartRequest struct {
	Profile            string
	CPUs               int
	MemoryGB           int
	DiskGB             int
	RootDiskGB         int
	MountInotify       bool
	SupplementalMounts []vm.Mount
	WorkspaceDir       string
	WorkspaceProvider  vm.WorkspaceProvider
	BrokerSpec         *broker.SessionSpec
	Verbose            bool
	AllowLowFDHeadroom bool
}

// RecoveryHandler may clear a diagnosed stale lock. A nil result authorizes
// one retry, while an error preserves the original failed-start outcome.
type RecoveryHandler func(vm.StaleLockRecoverer, string, *vm.StaleLockDiagnosis) error

// Coordinator is the only supported route to Backend.Start. Guards are run
// again before a stale-lock recovery retry because host pressure can change
// while the user is deciding whether to recover.
type Coordinator struct {
	Backend         vm.Backend
	Broker          broker.SyncBroker
	Sysctl          SysctlReader
	GOOS            string
	Stderr          io.Writer
	FDPolicy        FDPolicy
	WorkspacePolicy WorkspacePolicy
	Recover         RecoveryHandler
}

// NewCoordinator returns a coordinator with production guard dependencies.
func NewCoordinator(backend vm.Backend) *Coordinator {
	return &Coordinator{
		Backend:         backend,
		Sysctl:          CommandSysctlReader{},
		GOOS:            runtime.GOOS,
		Stderr:          os.Stderr,
		FDPolicy:        DefaultFDPolicy(),
		WorkspacePolicy: DefaultWorkspacePolicy(),
	}
}

// Start validates host safety, builds StartSpec, starts the backend, and offers
// a single conservative stale-lock recovery retry when configured.
func (c *Coordinator) Start(req StartRequest) error {
	if c.Backend == nil {
		return fmt.Errorf("starting VM %q: backend is required", req.Profile)
	}

	for attempt := 0; attempt < 2; attempt++ {
		spec, err := c.prepare(req)
		if err != nil {
			return err
		}
		err = c.Backend.Start(req.Profile, spec)
		if err == nil {
			if req.WorkspaceProvider == vm.BrokerWorkspace {
				return c.activateBroker(context.Background(), req.BrokerSpec, false)
			}
			return nil
		}
		if attempt == 1 || c.Recover == nil {
			return err
		}
		recoverer, ok := c.Backend.(vm.StaleLockRecoverer)
		if !ok {
			return err
		}
		diagnosis := recoverer.DiagnoseStartFailure(req.Profile)
		if diagnosis == nil {
			return err
		}
		if recoveryErr := c.Recover(recoverer, req.Profile, diagnosis); recoveryErr != nil {
			if errors.Is(recoveryErr, ErrRecoveryDeclined) {
				return err
			}
			return recoveryErr
		}
	}
	return nil
}

func (c *Coordinator) prepare(req StartRequest) (vm.StartSpec, error) {
	stderr := c.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	if c.GOOS == "darwin" {
		result, err := CheckFDHeadroom(c.Sysctl, c.fdPolicy())
		if err != nil {
			return vm.StartSpec{}, fmt.Errorf("pre-start file descriptor guard: %w", err)
		}
		if result.Warning != "" {
			fmt.Fprintln(stderr, result.Warning)
		}
		if result.Refuse && !req.AllowLowFDHeadroom {
			return vm.StartSpec{}, fmt.Errorf("pre-start file descriptor guard refused VM start: %s", result.Detail())
		}
	}
	if err := validateSupplementalMounts(req.WorkspaceDir, req.SupplementalMounts); err != nil {
		return vm.StartSpec{}, fmt.Errorf("pre-start mount guard: %w", err)
	}

	assessment, err := CheckWorkspace(req.WorkspaceDir, req.WorkspaceProvider, c.workspacePolicy())
	if err != nil {
		return vm.StartSpec{}, fmt.Errorf("pre-start workspace guard: %w", err)
	}
	if assessment.Warning != "" {
		fmt.Fprintln(stderr, assessment.Warning)
	}
	if req.WorkspaceProvider == vm.BrokerWorkspace {
		if c.Broker == nil || req.BrokerSpec == nil {
			return vm.StartSpec{}, fmt.Errorf("broker workspace provider requires a sync broker and session spec")
		}
		if err := c.preflightBroker(req.BrokerSpec); err != nil {
			return vm.StartSpec{}, err
		}
	}

	var workspaceMount *vm.Mount
	if req.WorkspaceProvider == vm.VirtiofsWorkspace {
		workspaceMount = &vm.Mount{Location: req.WorkspaceDir, Writable: true}
	}
	return vm.StartSpec{
		CPUs:               req.CPUs,
		MemoryGB:           req.MemoryGB,
		DiskGB:             req.DiskGB,
		RootDiskGB:         req.RootDiskGB,
		SupplementalMounts: append([]vm.Mount(nil), req.SupplementalMounts...),
		WorkspaceMount:     workspaceMount,
		MountInotify:       req.MountInotify && req.WorkspaceProvider != vm.BrokerWorkspace,
		WorkspaceProvider:  req.WorkspaceProvider,
		Verbose:            req.Verbose,
	}, nil
}

// ActivateBroker creates or resumes the project session and completes a clean
// flush barrier before callers can launch work inside the guest.
func (c *Coordinator) ActivateBroker(ctx context.Context, spec *broker.SessionSpec) error {
	return c.activateBroker(ctx, spec, true)
}

func (c *Coordinator) activateBroker(ctx context.Context, spec *broker.SessionSpec, runPreflight bool) error {
	if c.Broker == nil || spec == nil {
		return fmt.Errorf("activating broker workspace: broker and session spec are required")
	}
	if runPreflight {
		if err := c.preflightBroker(spec); err != nil {
			return err
		}
	}
	status, err := c.Broker.Status(ctx, *spec)
	if err != nil {
		return fmt.Errorf("checking existing workspace session: %w", err)
	}
	command, err := broker.GuestRootCommand(*spec, status.State == broker.StateMissing)
	if err != nil {
		return err
	}
	if _, err := c.Backend.SSHCommand(spec.Profile, command); err != nil {
		return fmt.Errorf("creating stable guest workspace %q: %w", spec.GuestRoot, err)
	}
	if err := c.Broker.Create(ctx, *spec); err != nil {
		_ = c.Broker.Pause(ctx, *spec)
		return fmt.Errorf("creating synchronized project copy: %w", err)
	}
	if err := c.FlushBroker(ctx, spec); err != nil {
		_ = c.Broker.Pause(ctx, *spec)
		return err
	}
	return nil
}

func (c *Coordinator) preflightBroker(spec *broker.SessionSpec) error {
	policy, err := brokerignore.Compile(spec.HostRoot, spec.Ignore)
	if err != nil {
		return fmt.Errorf("compiling broker ignore policy: %w", err)
	}
	report, err := broker.PreflightProject(spec.HostRoot, policy)
	if err != nil {
		return fmt.Errorf("broker project preflight: %w", err)
	}
	stderr := c.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	fmt.Fprintf(stderr, "Workspace broker mode uses a synchronized copy at %s, not local-filesystem equivalence.\n", spec.GuestRoot)
	for _, warning := range report.Warnings {
		fmt.Fprintf(stderr, "Warning: broker metadata preflight: %s. Extended attributes remain host-side.\n", warning)
	}
	return nil
}

// FlushBroker is the synchronization barrier used before guest execution and
// every operation that can stop or replace a VM.
func (c *Coordinator) FlushBroker(ctx context.Context, spec *broker.SessionSpec) error {
	if c.Broker == nil || spec == nil {
		return fmt.Errorf("flushing broker workspace: broker and session spec are required")
	}
	if err := c.Broker.Flush(ctx, *spec); err != nil {
		return fmt.Errorf("workspace flush failed, refusing clean continuation: %w", err)
	}
	status, err := c.Broker.Status(ctx, *spec)
	if err != nil {
		return fmt.Errorf("checking workspace status after flush: %w", err)
	}
	if err := status.Clean(); err != nil {
		return fmt.Errorf("workspace is not clean, refusing clean continuation: %w", err)
	}
	return nil
}

// QuiesceBroker flushes, verifies, pauses, and optionally terminates a session.
// Termination is reserved for destructive VM replacement where the beta copy
// and its synchronization history can no longer remain valid.
func (c *Coordinator) QuiesceBroker(ctx context.Context, spec *broker.SessionSpec, terminate bool) error {
	if err := c.FlushBroker(ctx, spec); err != nil {
		return err
	}
	if err := c.Broker.Pause(ctx, *spec); err != nil {
		return fmt.Errorf("pausing workspace session: %w", err)
	}
	if terminate {
		if err := c.Broker.Terminate(ctx, *spec); err != nil {
			return fmt.Errorf("terminating workspace session: %w", err)
		}
	}
	return nil
}

// Stop performs clean broker teardown before stopping the backend. A failed
// flush or unresolved conflict leaves the VM running and returns an error.
func (c *Coordinator) Stop(ctx context.Context, profile string, spec *broker.SessionSpec, terminate, verbose bool) error {
	if spec != nil {
		if err := c.QuiesceBroker(ctx, spec, terminate); err != nil {
			return err
		}
	}
	return c.Backend.Stop(profile, verbose)
}

func validateSupplementalMounts(workspaceDir string, mounts []vm.Mount) error {
	if workspaceDir == "" {
		return nil
	}
	workspaceDir = canonicalIfPresent(workspaceDir)
	for _, mount := range mounts {
		location := canonicalIfPresent(mount.Location)
		rel, err := filepath.Rel(location, workspaceDir)
		if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
			return fmt.Errorf("supplemental mount %q contains workspace %q", mount.Location, workspaceDir)
		}
	}
	return nil
}

func canonicalIfPresent(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

func (c *Coordinator) fdPolicy() FDPolicy {
	if c.FDPolicy.RefuseRatio == 0 {
		return DefaultFDPolicy()
	}
	return c.FDPolicy
}

func (c *Coordinator) workspacePolicy() WorkspacePolicy {
	if c.WorkspacePolicy.RefuseEntries == 0 {
		return DefaultWorkspacePolicy()
	}
	return c.WorkspacePolicy
}
