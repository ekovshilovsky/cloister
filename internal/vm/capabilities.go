package vm

import "time"

// GoldenImageManager defines lifecycle operations for base VM images used as
// clone sources. Implementations manage a single "base" image that is
// periodically refreshed and from which per-profile VMs are cloned or
// snapshot-reset, avoiding full OS provisioning on every profile creation.
type GoldenImageManager interface {
	// CreateBase provisions the base image from scratch, installing the OS and
	// any common tooling required by all derived profiles. When verbose is true,
	// provisioning output is forwarded to stdout. The presetOverride parameter
	// allows the caller to supply a custom unattended setup preset (a Lume
	// preset name or path to a YAML file). When empty, the backend selects the
	// correct preset automatically.
	CreateBase(verbose bool, presetOverride string) error

	// BaseExists reports whether a base image is present and available for
	// cloning. Callers should invoke this before CreateBase to avoid
	// unnecessary re-provisioning.
	BaseExists() bool

	// BaseAge returns the creation or last-refresh timestamp of the base image.
	// Callers use this to decide whether the base image is stale and should be
	// recreated (e.g. if it is older than a configurable retention period).
	BaseAge() time.Time

	// Clone creates a new VM instance at dest by copying the image at source.
	// Both source and dest are backend-specific instance identifiers (e.g.
	// cloister profile names). The dest instance must not already exist.
	Clone(source, dest string) error

	// Snapshot captures the current disk state of the given profile's VM under
	// the supplied name. The snapshot can later be used as a restoration point
	// via Reset.
	Snapshot(profile, name string) error

	// Reset reverts the given profile's VM to a prior state. When toFactory is
	// true, the VM is reset to the state of the golden base image; when false,
	// the most recent snapshot is used.
	Reset(profile string, toFactory bool) error

	// ListSnapshots returns metadata for all snapshots that exist for the given
	// profile's VM, ordered from oldest to most recent.
	ListSnapshots(profile string) ([]SnapshotInfo, error)
}

// SnapshotInfo describes a single VM snapshot captured by GoldenImageManager.
type SnapshotInfo struct {
	// Name is the user-supplied or auto-generated label for the snapshot.
	Name string

	// Created is the timestamp at which the snapshot was taken.
	Created time.Time
}

// NATNetworker provides IP address discovery for VMs that are reachable through
// the hypervisor's internal NAT network. Backends that expose a routable guest
// IP (e.g. Lume with a bridged or NAT interface) implement this interface.
type NATNetworker interface {
	// VMIP returns the current IP address assigned to the VM for the given
	// profile. It returns an error if the VM is not running or if the address
	// cannot be determined.
	VMIP(profile string) (string, error)
}

// DisplayManager controls the graphical display configuration of a VM. This is
// relevant for macOS guest VMs (e.g. managed by Lume) where a virtual display
// must be explicitly enabled before a VNC or screen-sharing connection can be
// established.
type DisplayManager interface {
	// EnableDisplay attaches a virtual display to the given profile's VM at the
	// specified resolution (e.g. "1920x1080"). The VM must already be running.
	EnableDisplay(profile string, resolution string) error

	// DisableDisplay detaches the virtual display from the given profile's VM,
	// freeing the associated GPU and framebuffer resources.
	DisableDisplay(profile string) error

	// VNCPort returns the host TCP port on which a VNC server is listening for
	// the given profile's VM. Returns an error if no display is active.
	VNCPort(profile string) (int, error)
}
