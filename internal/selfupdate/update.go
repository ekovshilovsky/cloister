package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	githubOwner = "ekovshilovsky"
	githubRepo  = "cloister"
)

// githubRelease is the subset of the GitHub releases API response that the
// self-update flow requires.
type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

// githubAsset represents a single downloadable artifact attached to a release.
type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Run checks the latest GitHub release and replaces the running binary when a
// newer version is available. currentVersion is compared against the release
// tag after stripping any leading "v" prefix from both sides.
func Run(currentVersion string) error {
	release, err := fetchLatestRelease()
	if err != nil {
		return fmt.Errorf("fetching latest release: %w", err)
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	normalised := strings.TrimPrefix(currentVersion, "v")

	if latestVersion == normalised {
		fmt.Println("Already up to date.")
		return nil
	}

	fmt.Printf("Updating cloister from %s to %s…\n", currentVersion, release.TagName)

	data, err := downloadReleaseBinary(release, latestVersion)
	if err != nil {
		return fmt.Errorf("downloading release binary: %w", err)
	}

	path, err := replaceBinary(data)
	if err != nil {
		return fmt.Errorf("replacing binary: %w", err)
	}

	fmt.Printf("Updated successfully: %s\n", path)
	return nil
}

// fetchLatestRelease queries the GitHub releases API for the most recent
// published release of the cloister repository.
func fetchLatestRelease() (*githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)

	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &release, nil
}

// downloadReleaseBinary locates the platform-appropriate tarball in the release
// assets, downloads it, and extracts the cloister binary from the archive.
// The tarball naming convention is: cloister_<version>_<os>_<arch>.tar.gz
func downloadReleaseBinary(release *githubRelease, version string) ([]byte, error) {
	targetName := fmt.Sprintf("cloister_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)

	var downloadURL string
	for _, asset := range release.Assets {
		if asset.Name == targetName {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}

	if downloadURL == "" {
		return nil, fmt.Errorf("no release asset found for platform %s/%s (expected %s)", runtime.GOOS, runtime.GOARCH, targetName)
	}

	resp, err := http.Get(downloadURL) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", downloadURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asset download returned HTTP %d", resp.StatusCode)
	}

	return extractBinaryFromTarGz(resp.Body)
}

// replaceBinary atomically replaces the currently running binary with the
// supplied content. It writes to a sibling temporary file and renames it over
// the original to avoid a partial-write window. It returns the resolved path
// of the binary that was replaced.
func replaceBinary(newBinary []byte) (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving executable path: %w", err)
	}

	// Follow symlinks so we overwrite the real file on disk.
	resolved, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", fmt.Errorf("resolving symlinks for %s: %w", exePath, err)
	}

	// Inherit the existing file's permissions so the replacement is executable.
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", resolved, err)
	}

	tmpPath := resolved + ".update"

	if err := os.WriteFile(tmpPath, newBinary, info.Mode()); err != nil {
		return "", fmt.Errorf("writing temporary binary %s: %w", tmpPath, err)
	}

	// Atomic rename: the OS swaps the directory entry in one syscall, so the
	// binary is never in a partially-written state from the perspective of other
	// processes.
	if err := os.Rename(tmpPath, resolved); err != nil {
		// Clean up the temp file if the rename fails.
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("renaming %s → %s: %w", tmpPath, resolved, err)
	}

	return resolved, nil
}

// extractBinaryFromTarGz reads a .tar.gz stream and returns the raw bytes of
// the file named "cloister" found inside the archive.
func extractBinaryFromTarGz(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("initialising gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar entry: %w", err)
		}

		// Match the bare binary name, ignoring any directory prefix in the
		// archive that a build tool may have introduced.
		if filepath.Base(hdr.Name) != "cloister" {
			continue
		}

		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading binary from archive: %w", err)
		}

		return data, nil
	}

	return nil, fmt.Errorf("archive does not contain a file named \"cloister\"")
}
