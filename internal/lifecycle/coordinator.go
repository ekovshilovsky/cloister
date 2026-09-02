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
	BrokerSpecs        []broker.SessionSpec
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
			if req.WorkspaceProvider.IsBroker() {
				return c.activateBrokers(context.Background(), brokerSpecs(req), false, hasCompleteBrokerCollection(req))
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
			return vm.StartSpec{}, fmt.Errorf("pre-start file descriptor guard refused VM start: %s; set CLOISTER_ALLOW_LOW_FD_HEADROOM=1 to override", result.Detail())
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
	if req.WorkspaceProvider.IsBroker() {
		specs := brokerSpecs(req)
		if c.Broker == nil || len(specs) == 0 {
			return vm.StartSpec{}, fmt.Errorf("broker workspace provider requires a sync broker and at least one session spec")
		}
		for i := range specs {
			if err := c.preflightBroker(&specs[i]); err != nil {
				return vm.StartSpec{}, fmt.Errorf("workspace project %q: %w", specs[i].HostRoot, err)
			}
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
		MountInotify:       req.MountInotify && !req.WorkspaceProvider.IsBroker(),
		WorkspaceProvider:  req.WorkspaceProvider,
		Verbose:            req.Verbose,
	}, nil
}

// ActivateBroker creates or resumes the project session and completes a clean
// flush barrier before callers can launch work inside the guest.
func (c *Coordinator) ActivateBroker(ctx context.Context, spec *broker.SessionSpec) error {
	if spec == nil {
		return fmt.Errorf("activating broker workspace: broker and session spec are required")
	}
	return c.activateBrokers(ctx, []broker.SessionSpec{*spec}, true, false)
}

// ActivateBrokers activates a complete workspace collection.
func (c *Coordinator) ActivateBrokers(ctx context.Context, specs []broker.SessionSpec) error {
	return c.activateBrokers(ctx, specs, true, true)
}

func (c *Coordinator) activateBrokers(ctx context.Context, specs []broker.SessionSpec, runPreflight, reconcile bool) error {
	if c.Broker == nil || len(specs) == 0 {
		return fmt.Errorf("activating broker workspace: broker and session specs are required")
	}
	if runPreflight {
		for i := range specs {
			if err := c.preflightBroker(&specs[i]); err != nil {
				return fmt.Errorf("workspace project %q: %w", specs[i].HostRoot, err)
			}
		}
	}
	if reconcile {
		if reconciler, ok := c.Broker.(broker.ProfileReconciler); ok {
			profile := specs[0].Profile
			for i := range specs {
				if specs[i].Profile != profile {
					return fmt.Errorf("activating broker workspace collection: session profiles do not match")
				}
			}
			if err := reconciler.ReconcileProfile(ctx, profile, specs); err != nil {
				return fmt.Errorf("reconciling broker workspace collection for profile %q: %w", profile, err)
			}
		}
	}
	var touched []broker.SessionSpec
	rollback := func() {
		for i := len(touched) - 1; i >= 0; i-- {
			_ = c.Broker.Pause(ctx, touched[i])
		}
	}
	for i := range specs {
		spec := &specs[i]
		status, err := c.Broker.Status(ctx, *spec)
		if err != nil {
			rollback()
			return fmt.Errorf("workspace project %q: checking existing session: %w", spec.HostRoot, err)
		}
		// Choose how to prepare the managed guest root so a leftover directory
		// never dead-ends activation and never silently resurrects host state:
		//   - No live session (missing): reset the guest root. Any content is a
		//     stale copy from a terminated session or restored snapshot; the host
		//     is authoritative, so clearing avoids a one-sided guest copy
		//     re-creating host-deleted files under two-way-safe.
		//   - Existing session (paused/active): adopt the guest root as-is; its
		//     synchronization history is still valid.
		var command string
		if status.State == broker.StateMissing {
			command, err = broker.GuestRootResetCommand(*spec)
		} else {
			command, err = broker.GuestRootCommand(*spec, false)
		}
		if err != nil {
			rollback()
			return err
		}
		if _, err := c.Backend.SSHScript(spec.Profile, command); err != nil {
			rollback()
			return fmt.Errorf("workspace project %q: creating stable guest root %q: %w", spec.HostRoot, spec.GuestRoot, err)
		}
		if err := c.Broker.Create(ctx, *spec); err != nil {
			touched = append(touched, *spec)
			rollback()
			return fmt.Errorf("workspace project %q: creating synchronized copy: %w", spec.HostRoot, err)
		}
		touched = append(touched, *spec)
		if err := c.FlushBroker(ctx, spec); err != nil {
			rollback()
			return fmt.Errorf("workspace project %q: %w", spec.HostRoot, err)
		}
	}
	return nil
}

func brokerSpecs(req StartRequest) []broker.SessionSpec {
	if hasCompleteBrokerCollection(req) {
		return append([]broker.SessionSpec(nil), req.BrokerSpecs...)
	}
	if req.BrokerSpec != nil {
		return []broker.SessionSpec{*req.BrokerSpec}
	}
	return nil
}

func hasCompleteBrokerCollection(req StartRequest) bool {
	return req.BrokerSpecs != nil
}

func (c *Coordinator) preflightBroker(spec *broker.SessionSpec) error {
	policy, err := broker.CompilePolicy(*spec)
	if err != nil {
		return fmt.Errorf("compiling broker ignore policy: %w", err)
	}
	report, err := broker.PreflightProjectWithLimit(spec.HostRoot, policy, spec.MaxEntries)
	if err != nil {
		return fmt.Errorf("broker project preflight: %w", err)
	}
	stderr := c.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	// Only findings specific to this project belong here: it runs once per
	// project and a collection holds dozens. The profile-wide fact that broker
	// mode is a synchronized copy is stated once per profile by
	// warnBrokerGitOnce.
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
	if spec == nil {
		return fmt.Errorf("quiescing broker workspace: session spec is required")
	}
	return c.QuiesceBrokers(ctx, []broker.SessionSpec{*spec}, terminate)
}

// QuiesceBrokers establishes a clean barrier for the complete collection
// before pausing or terminating any session.
func (c *Coordinator) QuiesceBrokers(ctx context.Context, specs []broker.SessionSpec, terminate bool) error {
	if len(specs) > 0 && c.Broker == nil {
		return fmt.Errorf("quiescing broker workspace: broker is required")
	}
	paused := make([]bool, len(specs))
	for i := range specs {
		status, err := c.Broker.Status(ctx, specs[i])
		if err != nil {
			return fmt.Errorf("workspace project %q: checking session before quiesce: %w", specs[i].HostRoot, err)
		}
		if status.State == broker.StatePaused {
			if err := status.Clean(); err != nil {
				return fmt.Errorf("workspace project %q: paused session is not clean: %w", specs[i].HostRoot, err)
			}
			paused[i] = true
			continue
		}
		if err := c.FlushBroker(ctx, &specs[i]); err != nil {
			return fmt.Errorf("workspace project %q: %w", specs[i].HostRoot, err)
		}
	}
	for i := range specs {
		if paused[i] {
			continue
		}
		if err := c.Broker.Pause(ctx, specs[i]); err != nil {
			return fmt.Errorf("workspace project %q: pausing session: %w", specs[i].HostRoot, err)
		}
	}
	if terminate {
		for i := range specs {
			if err := c.Broker.Terminate(ctx, specs[i]); err != nil {
				return fmt.Errorf("workspace project %q: terminating session: %w", specs[i].HostRoot, err)
			}
		}
	}
	return nil
}

// Stop performs clean broker teardown before stopping the backend. A failed
// flush or unresolved conflict leaves the VM running and returns an error.
func (c *Coordinator) Stop(ctx context.Context, profile string, spec *broker.SessionSpec, terminate, verbose bool) error {
	var specs []broker.SessionSpec
	if spec != nil {
		specs = []broker.SessionSpec{*spec}
	}
	return c.StopBrokers(ctx, profile, specs, terminate, verbose)
}

// StopBrokers cleanly tears down a workspace collection before VM stop.
func (c *Coordinator) StopBrokers(ctx context.Context, profile string, specs []broker.SessionSpec, terminate, verbose bool) error {
	if len(specs) > 0 {
		if err := c.QuiesceBrokers(ctx, specs, terminate); err != nil {
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
