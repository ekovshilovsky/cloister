package lume

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/vm"
)

// BaseImageName is the shared macOS base image used as the clone source for all
// OpenClaw profile VMs. A single base image is maintained per machine so that
// the 15-20 minute IPSW restore is only performed once.
const BaseImageName = "cloister-base-macos"

// baseImageName aliases the exported constant for internal use so that existing
// unexported references inside this package continue to read naturally.
const baseImageName = BaseImageName

// CreateBase provisions the shared macOS base image from a fresh IPSW restore.
// This operation installs the latest macOS Sequoia release and typically takes
// 15-20 minutes on first run. Subsequent profile creates clone this image in
// approximately two minutes. When verbose is true, Lume's output is forwarded
// to stderr so the caller can observe restore progress in real time.
func (b *Backend) CreateBase(verbose bool) error {
	return runLume(verbose, "create", baseImageName, "--os", "macos", "--ipsw", "latest", "--unattended", "sequoia")
}

// BaseExists reports whether the shared base image is registered with Lume.
// Callers invoke this before CreateBase to avoid unnecessary re-provisioning.
func (b *Backend) BaseExists() bool {
	cmd := exec.Command("lume", "get", baseImageName, "--format", "json")
	return cmd.Run() == nil
}

// BaseAge returns the creation timestamp of the shared base image by querying
// the Lume metadata for the base image VM. Returns a zero time.Time if the
// base image does not exist or its metadata cannot be parsed, allowing callers
// to treat a zero value as "unknown / force refresh".
func (b *Backend) BaseAge() time.Time {
	out, err := lumeGetJSON(baseImageName)
	if err != nil {
		return time.Time{}
	}
	var v lumeVM
	if err := json.Unmarshal(out, &v); err != nil {
		return time.Time{}
	}
	return v.Created
}

// Clone creates an independent copy of a Lume VM named source at the
// destination name dest. Both arguments are raw Lume VM names (not cloister
// profile names). The destination VM must not already exist. The source VM
// must be stopped before cloning.
func (b *Backend) Clone(source, dest string) error {
	return runLume(false, "clone", source, dest)
}

// Snapshot captures the current disk state of the given profile's VM under the
// supplied name. Internally this clones the running VM into a new Lume instance
// named <vmName>-<name>. The name "factory" is reserved by cloister for the
// automatic post-provisioning snapshot; all other names are available for
// user-defined checkpoints. The VM must be stopped by the caller before
// invoking this method.
func (b *Backend) Snapshot(profile, name string) error {
	src := VMName(profile)
	dest := fmt.Sprintf("%s-%s", src, name)
	return b.Clone(src, dest)
}

// Reset reverts the given profile's VM to a prior snapshot state. When
// toFactory is true the factory snapshot is used (captured automatically after
// provisioning); otherwise the user snapshot is used. The method deletes the
// current VM before cloning from the snapshot, so the caller must ensure the
// VM is stopped. If the target snapshot does not exist, an error is returned
// and the current VM is left intact.
func (b *Backend) Reset(profile string, toFactory bool) error {
	name := VMName(profile)
	suffix := "user"
	if toFactory {
		suffix = "factory"
	}
	snapshot := fmt.Sprintf("%s-%s", name, suffix)

	// Verify the target snapshot is registered before destroying the current VM.
	cmd := exec.Command("lume", "get", snapshot, "--format", "json")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("snapshot %q not found — cannot reset", snapshot)
	}

	// Remove the current VM. Ignore the error here; if deletion fails the
	// subsequent clone will surface a more actionable error message.
	_ = runLume(false, "delete", name, "--force")

	return b.Clone(snapshot, name)
}

// ListSnapshots returns metadata for all snapshots that exist for the given
// profile, ordered as Lume reports them. Snapshot VMs follow the naming
// convention <vmName>-<suffix>, where the suffix is a user-supplied or
// automatically assigned label. The raw Lume list is filtered to include only
// entries that match this prefix, and each matching VM's name suffix is
// returned as the snapshot name together with its creation timestamp.
func (b *Backend) ListSnapshots(profile string) ([]vm.SnapshotInfo, error) {
	vmName := VMName(profile)
	prefix := vmName + "-"

	var buf bytes.Buffer
	cmd := exec.Command("lume", "ls", "--format", "json")
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("lume ls: %w\n%s", err, buf.String())
	}

	trimmed := bytes.TrimSpace(buf.Bytes())
	if len(trimmed) == 0 {
		return nil, nil
	}

	var all []lumeVM
	if err := json.Unmarshal(trimmed, &all); err != nil {
		return nil, fmt.Errorf("parsing lume ls output: %w", err)
	}

	var snapshots []vm.SnapshotInfo
	for _, v := range all {
		if !strings.HasPrefix(v.Name, prefix) {
			continue
		}
		snapshots = append(snapshots, vm.SnapshotInfo{
			Name:    strings.TrimPrefix(v.Name, prefix),
			Created: v.Created,
		})
	}
	return snapshots, nil
}
