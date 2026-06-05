package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cloister.io/internal/config"
)

// driftKind classifies the relationship between the configured disk size and
// the actual on-disk image size for a single disk attached to a profile.
type driftKind int

const (
	// driftNone: configured size equals the on-disk image size. Nothing to do.
	driftNone driftKind = iota
	// driftShrink: configured size is smaller than the on-disk image size.
	// Passing the configured value to colima would trigger
	// "disk shrinking is not supported" and fail the start. The user must
	// either update config to match the on-disk size or recover the disk
	// image to the configured size manually.
	driftShrink
	// driftGrow: configured size is larger than the on-disk image size.
	// Colima will sparse-extend the image during start (no data loss); this
	// is the same in-place grow that `cloister resize` performs.
	driftGrow
)

const bytesPerGiB int64 = 1 << 30

// diskReconcileInteractive is the TTY-detection hook used by the disk-drift
// reconciler. It defaults to the package-level isInteractive helper (which
// inspects os.Stdout's mode for a character-device bit) but is exported as a
// var so tests can replace it with a deterministic stub — the unit tests
// feed prompt answers through a pipe, which does not appear as a character
// device, and there's no portable way to fake an isatty(3) result against a
// pipe descriptor.
var diskReconcileInteractive = isInteractive

// diskDrift records the comparison result for one disk (either root or data)
// on a single profile. The lima role name is captured verbatim so error
// messages match what the user sees in colima/lima docs.
type diskDrift struct {
	role         string // "root" or "data"
	actualGB     int64
	configuredGB int
	kind         driftKind
}

// detectDriftForProfile inspects the on-disk image files for the given
// cloister profile and returns one diskDrift per disk that exists on disk.
// The profile's configured Disk and RootDisk values are taken as the ground
// truth that backend.Start will pass to colima at start time. Files that
// don't exist (e.g. data disk on a legacy single-disk profile) are skipped
// silently rather than synthesised as drift, because their absence means the
// user has never opted into that disk and so there is nothing to reconcile.
func detectDriftForProfile(profileName string, p *config.Profile) ([]diskDrift, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}
	vmName := "colima-cloister-" + profileName

	var out []diskDrift

	// Root disk: per-instance, always present for any started colima VM.
	rootPath := filepath.Join(home, ".colima", "_lima", vmName, "disk")
	if fi, err := os.Stat(rootPath); err == nil {
		actualGB := fi.Size() / bytesPerGiB
		if actualGB > 0 {
			out = append(out, classifyDrift("root", actualGB, p.RootDisk))
		}
	}

	// Data disk: lives in lima's shared pool. Legacy single-disk profiles
	// don't have one — silently skip when the file isn't there.
	dataPath := filepath.Join(home, ".colima", "_lima", "_disks", vmName, "datadisk")
	if fi, err := os.Stat(dataPath); err == nil {
		actualGB := fi.Size() / bytesPerGiB
		if actualGB > 0 {
			out = append(out, classifyDrift("data", actualGB, p.Disk))
		}
	}

	return out, nil
}

// classifyDrift compares actual vs configured size for a single disk role
// and returns the corresponding diskDrift. A configuredGB of zero means
// "use the backend's default", which is treated as a no-drift case to avoid
// mis-classifying profiles that opt out of an explicit size.
func classifyDrift(role string, actualGB int64, configuredGB int) diskDrift {
	d := diskDrift{role: role, actualGB: actualGB, configuredGB: configuredGB}
	switch {
	case configuredGB == 0:
		d.kind = driftNone
	case int64(configuredGB) == actualGB:
		d.kind = driftNone
	case int64(configuredGB) < actualGB:
		d.kind = driftShrink
	default:
		d.kind = driftGrow
	}
	return d
}

// reconcileDiskDrift inspects the profile's disks for size drift and, when
// drift is found, prompts the user to decide how to resolve it. The function
// is called from cmd/enter.go before backend.Start so the user has a chance
// to fix a drift that would otherwise cause colima to fail with
// "disk shrinking is not supported" (or to silently grow a disk the user did
// not intend to change).
//
// Resolution semantics, matching the user-stated policy:
//
//   - shrink-direction drift (actual > configured): prompt to update the
//     profile config to match the on-disk size. Accepting writes back to
//     ~/.cloister/config.yaml so subsequent starts agree. Declining returns
//     an error so the start aborts and the user can fix manually.
//   - grow-direction drift (actual < configured): prompt to allow colima to
//     grow the image during start. Declining returns an error so the start
//     aborts; we explicitly do not silently revert the profile.
//   - no drift: returns nil with no output.
//
// When stdin is not a TTY (scripted invocation) the function declines to
// guess and returns an error describing the drift, mirroring the user's
// "don't do anything in silence" guideline.
func reconcileDiskDrift(out io.Writer, cfgPath string, cfg *config.Config, profileName string, p *config.Profile) error {
	drifts, err := detectDriftForProfile(profileName, p)
	if err != nil {
		// Detection failure (e.g. unreadable home directory) is not fatal —
		// returning nil here lets backend.Start surface the underlying
		// problem with its own error path instead of double-reporting.
		fmt.Fprintf(out, "warning: skipping disk-drift check: %v\n", err)
		return nil
	}

	var actionable []diskDrift
	for _, d := range drifts {
		if d.kind != driftNone {
			actionable = append(actionable, d)
		}
	}
	if len(actionable) == 0 {
		return nil
	}

	fmt.Fprintf(out, "\nDisk size drift detected for profile %q:\n", profileName)
	for _, d := range actionable {
		fmt.Fprintf(out, "  %s disk: %d GiB on disk vs %d GiB in config — %s\n",
			d.role, d.actualGB, d.configuredGB, driftKindDescription(d.kind))
	}
	fmt.Fprintln(out)

	hasShrink := containsKind(actionable, driftShrink)
	hasGrow := containsKind(actionable, driftGrow)

	if !diskReconcileInteractive() {
		return fmt.Errorf("disk-drift detected and stdin is not a TTY — re-run interactively to resolve, or fix the profile config manually")
	}

	reader := bufio.NewReader(os.Stdin)

	switch {
	case hasShrink && hasGrow:
		// Mixed: handle each disk independently so the user sees a prompt
		// scoped to a single decision instead of a compound one.
		return resolveEachDrift(out, reader, cfgPath, cfg, p, actionable)
	case hasShrink:
		return resolveShrinkDrifts(out, reader, cfgPath, cfg, p, actionable)
	case hasGrow:
		return resolveGrowDrifts(out, reader, actionable)
	}

	return nil
}

// driftKindDescription returns the user-facing label for a driftKind so the
// summary line above the prompt is unambiguous about the direction and
// consequence of each individual drift.
func driftKindDescription(k driftKind) string {
	switch k {
	case driftShrink:
		return "would require shrinking (colima refuses)"
	case driftGrow:
		return "colima would grow the image to match"
	default:
		return "in sync"
	}
}

// containsKind reports whether any drift in the slice has the given kind.
// Small helper used to gate the prompt selection logic above.
func containsKind(drifts []diskDrift, k driftKind) bool {
	for _, d := range drifts {
		if d.kind == k {
			return true
		}
	}
	return false
}

// resolveShrinkDrifts prompts the user to update the profile config to match
// the actual on-disk sizes. Accepting writes the updated profile back to the
// cloister config file; declining returns an error so the start aborts.
// The prompt is phrased once for all shrink-direction drifts because the
// recommended action is the same for all of them.
func resolveShrinkDrifts(out io.Writer, reader *bufio.Reader, cfgPath string, cfg *config.Config, p *config.Profile, drifts []diskDrift) error {
	fmt.Fprint(out, "Update profile config to match the actual disk sizes? (alternative: abort and reconcile manually) [Y/n]: ")
	ans, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if !isYes(ans) {
		return fmt.Errorf("aborted by user — update %s manually or rebuild the profile", cfgPath)
	}

	for _, d := range drifts {
		if d.kind != driftShrink {
			continue
		}
		applyDriftToProfile(p, d)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Fprintln(out, "✓ Profile config updated to match on-disk sizes.")
	return nil
}

// resolveGrowDrifts confirms with the user that colima should sparse-extend
// the disk image during start. Declining returns an error so the user can
// edit the profile or run cloister resize on their own terms; we explicitly
// don't auto-shrink the config without consent.
func resolveGrowDrifts(out io.Writer, reader *bufio.Reader, drifts []diskDrift) error {
	fmt.Fprint(out, "Allow colima to grow the disk(s) to the configured size during start? [Y/n]: ")
	ans, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if !isYes(ans) {
		return fmt.Errorf("aborted by user — adjust the profile config or run `cloister resize` instead")
	}
	fmt.Fprintln(out, "✓ Proceeding with disk grow on next start.")
	return nil
}

// resolveEachDrift handles the rare mixed case where one disk would shrink
// and the other would grow on the same start. Each disk gets its own prompt
// so the user does not have to give a compound answer that ambiguously
// covers two distinct actions.
func resolveEachDrift(out io.Writer, reader *bufio.Reader, cfgPath string, cfg *config.Config, p *config.Profile, drifts []diskDrift) error {
	changed := false
	for _, d := range drifts {
		switch d.kind {
		case driftShrink:
			fmt.Fprintf(out, "Update %s disk config from %d to %d GiB to match the on-disk image? [Y/n]: ",
				d.role, d.configuredGB, d.actualGB)
			ans, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("reading confirmation: %w", err)
			}
			if !isYes(ans) {
				return fmt.Errorf("aborted by user — %s disk drift unresolved", d.role)
			}
			applyDriftToProfile(p, d)
			changed = true
		case driftGrow:
			fmt.Fprintf(out, "Allow colima to grow %s disk from %d to %d GiB during start? [Y/n]: ",
				d.role, d.actualGB, d.configuredGB)
			ans, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("reading confirmation: %w", err)
			}
			if !isYes(ans) {
				return fmt.Errorf("aborted by user — %s disk grow declined", d.role)
			}
		}
	}
	if changed {
		if err := config.Save(cfgPath, cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Fprintln(out, "✓ Profile config updated to match on-disk sizes.")
	}
	return nil
}

// applyDriftToProfile mutates the in-memory profile to set the configured
// size for the drifting role to the on-disk size. The caller is responsible
// for persisting the change via config.Save.
func applyDriftToProfile(p *config.Profile, d diskDrift) {
	switch d.role {
	case "root":
		p.RootDisk = int(d.actualGB)
	case "data":
		p.Disk = int(d.actualGB)
	}
}

// isYes returns true when the user's answer to a [Y/n] prompt should be
// interpreted as yes. Empty input accepts the default (yes); any input
// starting with "y" also accepts. Everything else (including "n", "no",
// random text) is treated as no.
func isYes(answer string) bool {
	a := strings.TrimSpace(strings.ToLower(answer))
	return a == "" || strings.HasPrefix(a, "y")
}

