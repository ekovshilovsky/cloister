package lume

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// CheckHostCompatibility verifies that the host macOS version is recent enough
// to install the latest IPSW restore image. Apple's Virtualization.framework
// requires the host OS to be at least the same version as the guest. This check
// runs before downloading the ~13GB IPSW so users are not blocked after a long
// wait.
//
// Returns nil if compatible, or an error describing the version mismatch and
// the required action.
func CheckHostCompatibility() error {
	// Get the latest IPSW URL which contains the macOS version in the filename
	// (e.g. UniversalMac_26.4_25E246_Restore.ipsw)
	ipswOut, err := exec.Command("lume", "ipsw").CombinedOutput()
	if err != nil {
		// If we can't determine the IPSW version, proceed anyway — CreateBase
		// will surface the real error from Virtualization.framework.
		return nil
	}

	ipswVersion := parseIPSWVersion(string(ipswOut))
	if ipswVersion == "" {
		return nil // Could not parse version, let CreateBase handle errors
	}

	// Get the host macOS version
	hostOut, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return nil // Could not determine host version, proceed anyway
	}
	hostVersion := strings.TrimSpace(string(hostOut))

	if !versionAtLeast(hostVersion, ipswVersion) {
		return fmt.Errorf(
			"macOS %s required but this host is running %s.\n"+
				"The latest IPSW restore image requires the host OS to be at least the same version.\n"+
				"Update macOS: softwareupdate --install --all\n"+
				"Then retry: cloister create --openclaw <name>",
			ipswVersion, hostVersion,
		)
	}

	return nil
}

// parseIPSWVersion extracts the macOS version from lume ipsw output.
// The URL contains a filename like UniversalMac_26.4_25E246_Restore.ipsw.
func parseIPSWVersion(output string) string {
	// Look for UniversalMac_XX.Y pattern in the URL
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "UniversalMac_") {
			continue
		}
		// Extract version between "UniversalMac_" and the next "_"
		idx := strings.Index(line, "UniversalMac_")
		if idx < 0 {
			continue
		}
		rest := line[idx+len("UniversalMac_"):]
		if endIdx := strings.Index(rest, "_"); endIdx > 0 {
			return rest[:endIdx]
		}
	}
	return ""
}

// versionAtLeast returns true if the host version is >= the required version.
// Both are expected in major.minor format (e.g. "26.4").
func versionAtLeast(host, required string) bool {
	hostParts := strings.Split(host, ".")
	reqParts := strings.Split(required, ".")

	for i := 0; i < len(reqParts); i++ {
		if i >= len(hostParts) {
			return false // host has fewer components
		}
		h := 0
		r := 0
		fmt.Sscanf(hostParts[i], "%d", &h)
		fmt.Sscanf(reqParts[i], "%d", &r)
		if h > r {
			return true
		}
		if h < r {
			return false
		}
	}
	return true // equal
}

// CreateBase provisions the shared macOS base image from a fresh IPSW restore.
// This operation installs the latest macOS release and typically takes 15-20
// minutes on first run. Subsequent profile creates clone this image in
// approximately two minutes. When verbose is true, Lume's output is streamed
// to stdout so the user can observe progress in real time.
//
// The presetOverride parameter allows the caller to supply a custom unattended
// setup preset (a Lume preset name like "tahoe" or a path to a YAML file).
// When empty, cloister automatically selects the correct preset for the host
// macOS version — using its own embedded fixed YAML for versions where Lume's
// built-in preset is broken.
//
// The IPSW restore image (~13-18GB) is cached at ~/.cloister/cache/ipsw/ so
// that failed or repeated creates do not re-download. The cache is validated
// by comparing the local file size against the remote Content-Length header.
// A SHA-256 checksum file is stored alongside the IPSW for additional integrity
// verification when the file is reused from cache.
//
// Callers should invoke CheckHostCompatibility before CreateBase to catch
// version mismatches before the ~13GB IPSW download begins.
func (b *Backend) CreateBase(verbose bool, presetOverride string) error {
	preset := presetOverride
	if preset == "" {
		var err error
		preset, err = detectUnattendedPreset()
		if err != nil {
			return fmt.Errorf("selecting unattended setup preset: %w", err)
		}
	}
	fmt.Printf("Using unattended preset: %s\n", preset)

	// Resolve the IPSW path: use cached file if valid, otherwise download.
	ipswPath, err := resolveIPSW(verbose)
	if err != nil {
		// Fall back to letting Lume handle the download directly.
		fmt.Fprintf(os.Stderr, "Warning: IPSW cache unavailable (%v), Lume will download directly\n", err)
		return runLume(verbose, "create", baseImageName, "--os", "macos", "--ipsw", "latest", "--unattended", preset)
	}

	return runLume(verbose, "create", baseImageName, "--os", "macos", "--ipsw", ipswPath, "--unattended", preset)
}

// ipswCacheDir returns the directory used to cache IPSW restore images.
func ipswCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cloister", "cache", "ipsw")
}

// resolveIPSW returns the path to a validated local IPSW file. If the cache
// contains a valid copy that matches the latest remote IPSW, it is returned
// immediately. Otherwise the file is downloaded and cached for future use.
func resolveIPSW(verbose bool) (string, error) {
	// Get the latest IPSW URL from Lume.
	ipswURL, err := latestIPSWURL()
	if err != nil {
		return "", fmt.Errorf("resolving IPSW URL: %w", err)
	}

	// Derive a stable filename from the URL (e.g. UniversalMac_26.4_25E246_Restore.ipsw).
	filename := filepath.Base(ipswURL)
	if filename == "" || filename == "." {
		return "", fmt.Errorf("could not extract filename from IPSW URL: %s", ipswURL)
	}

	cacheDir := ipswCacheDir()
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", fmt.Errorf("creating IPSW cache directory: %w", err)
	}

	localPath := filepath.Join(cacheDir, filename)
	checksumPath := localPath + ".sha256"

	// Check if a cached copy exists and is valid.
	if isIPSWCacheValid(localPath, checksumPath, ipswURL) {
		fmt.Println("Using cached IPSW restore image.")
		return localPath, nil
	}

	// Download the IPSW to the cache directory.
	fmt.Printf("Downloading IPSW restore image to cache (%s)...\n", filename)
	if err := downloadIPSW(ipswURL, localPath, verbose); err != nil {
		// Clean up partial download.
		os.Remove(localPath)
		os.Remove(checksumPath)
		return "", fmt.Errorf("downloading IPSW: %w", err)
	}

	// Compute and store the SHA-256 checksum for future validation.
	checksum, err := fileSHA256(localPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not compute IPSW checksum: %v\n", err)
	} else {
		os.WriteFile(checksumPath, []byte(checksum), 0o600) //nolint:errcheck
	}

	return localPath, nil
}

// latestIPSWURL calls `lume ipsw` to resolve the download URL for the latest
// supported macOS restore image.
func latestIPSWURL() (string, error) {
	out, err := exec.Command("lume", "ipsw").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("lume ipsw: %w\n%s", err, string(out))
	}
	// The last non-empty line of output is the URL.
	for _, line := range reverseLines(string(out)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http") {
			return line, nil
		}
	}
	return "", fmt.Errorf("no URL found in lume ipsw output")
}

// reverseLines returns the lines of s in reverse order, for scanning output
// from the bottom (where the URL typically appears).
func reverseLines(s string) []string {
	lines := strings.Split(s, "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines
}

// isIPSWCacheValid checks whether the cached IPSW file is complete and matches
// the expected content. Validation strategy:
//  1. File must exist and be non-empty.
//  2. If a .sha256 checksum file exists, verify the file hash matches.
//  3. If no checksum file, compare file size against the remote Content-Length.
func isIPSWCacheValid(localPath, checksumPath, remoteURL string) bool {
	info, err := os.Stat(localPath)
	if err != nil || info.Size() == 0 {
		return false
	}

	// Strategy 1: checksum file exists — verify hash.
	if savedChecksum, err := os.ReadFile(checksumPath); err == nil {
		currentChecksum, err := fileSHA256(localPath)
		if err != nil {
			return false
		}
		if strings.TrimSpace(string(savedChecksum)) == currentChecksum {
			return true
		}
		// Checksum mismatch — file is corrupt or was partially overwritten.
		fmt.Println("Cached IPSW checksum mismatch — re-downloading.")
		return false
	}

	// Strategy 2: no checksum file — compare file size against remote.
	remoteSize, err := remoteContentLength(remoteURL)
	if err != nil {
		// Can't verify — assume the file is valid if it's reasonably large (>1GB).
		return info.Size() > 1<<30
	}
	if info.Size() == remoteSize {
		return true
	}

	fmt.Printf("Cached IPSW size mismatch (local=%d, remote=%d) — re-downloading.\n",
		info.Size(), remoteSize)
	return false
}

// remoteContentLength sends a HEAD request to the URL and returns the
// Content-Length header value. Returns an error if the request fails or the
// header is absent.
func remoteContentLength(url string) (int64, error) {
	resp, err := http.Head(url) //nolint:noctx
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.ContentLength <= 0 {
		return 0, fmt.Errorf("no Content-Length header")
	}
	return resp.ContentLength, nil
}

// downloadIPSW downloads the IPSW from url to localPath, showing progress
// when verbose is true.
func downloadIPSW(url, localPath string, verbose bool) error {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return fmt.Errorf("requesting IPSW: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("IPSW download returned HTTP %d", resp.StatusCode)
	}

	tmpPath := localPath + ".download"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	var reader io.Reader = resp.Body
	if verbose && resp.ContentLength > 0 {
		reader = &progressReader{
			reader: resp.Body,
			total:  resp.ContentLength,
			label:  "IPSW download",
		}
	}

	written, err := io.Copy(f, reader)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("writing IPSW: %w", err)
	}

	if resp.ContentLength > 0 && written != resp.ContentLength {
		os.Remove(tmpPath)
		return fmt.Errorf("incomplete download: got %d bytes, expected %d", written, resp.ContentLength)
	}

	// Atomic rename from temp to final path.
	if err := os.Rename(tmpPath, localPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("finalizing IPSW: %w", err)
	}

	if verbose {
		fmt.Printf("\nIPSW cached at %s\n", localPath)
	}
	return nil
}

// fileSHA256 computes the SHA-256 hash of the file at path and returns it as
// a lowercase hex string.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// progressReader wraps an io.Reader and prints download progress to stderr.
type progressReader struct {
	reader  io.Reader
	total   int64
	read    int64
	label   string
	lastPct int
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)
	pct := int(pr.read * 100 / pr.total)
	if pct != pr.lastPct && pct%5 == 0 {
		fmt.Fprintf(os.Stderr, "  %s: %d%%\n", pr.label, pct)
		pr.lastPct = pct
	}
	return n, err
}

// detectUnattendedPreset returns the Lume unattended setup preset name that
// matches the host macOS version. Each major macOS release changes the Setup
// Assistant UI, so the automation scripts must match the OS version.
//go:embed presets/tahoe-26.4.yml
var tahoe264Preset []byte

// detectUnattendedPreset returns the preset identifier or file path for the
// Lume unattended setup. For macOS versions where Lume's built-in preset works,
// it returns the preset name (e.g. "sequoia", "tahoe"). For versions where the
// built-in preset is broken (26.4+), it writes cloister's embedded fixed YAML
// to a temp file and returns the path. Returns an error if the preset cannot
// be resolved — there is no fallback, because a wrong preset causes a silent
// failure deep in the unattended setup.
func detectUnattendedPreset() (string, error) {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return "", fmt.Errorf("could not determine macOS version: %w", err)
	}
	version := strings.TrimSpace(string(out))
	parts := strings.Split(version, ".")
	if len(parts) == 0 {
		return "", fmt.Errorf("could not parse macOS version: %q", version)
	}
	major := 0
	minor := 0
	fmt.Sscanf(parts[0], "%d", &major)
	if len(parts) > 1 {
		fmt.Sscanf(parts[1], "%d", &minor)
	}

	// macOS 26.4+ — Lume's built-in tahoe preset is broken (Apple renamed
	// "Set Up Later" to "Other Sign-In Options" → "Sign in Later in Settings",
	// and added a new "Age Range" screen). Use cloister's embedded fixed YAML.
	if major > 26 || (major == 26 && minor >= 4) {
		path, err := writeCustomPreset(tahoe264Preset, "tahoe-26.4")
		if err != nil {
			return "", fmt.Errorf("could not write custom preset for macOS %s: %w", version, err)
		}
		return path, nil
	}

	// macOS 26.0-26.3 — Lume's built-in tahoe preset works.
	if major >= 26 {
		return "tahoe", nil
	}

	// macOS 15.x (Sequoia) — Lume's built-in sequoia preset works.
	return "sequoia", nil
}

// writeCustomPreset writes the embedded preset YAML to a temp file and returns
// the path. Lume's --unattended flag accepts either a preset name or a file
// path. Returns an error if the file cannot be written.
func writeCustomPreset(data []byte, name string) (string, error) {
	dir := os.TempDir()
	path := filepath.Join(dir, fmt.Sprintf("cloister-%s.yml", name))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("writing preset %q to %s: %w", name, path, err)
	}
	return path, nil
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
	v, err := lumeGetVM(baseImageName)
	if err != nil {
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
