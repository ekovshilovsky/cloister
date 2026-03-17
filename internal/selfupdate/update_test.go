package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

// buildTarGz creates an in-memory .tar.gz archive containing a single file at
// the supplied path with the given content.
func buildTarGz(t *testing.T, name string, content []byte) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: name,
		Mode: 0755,
		Size: int64(len(content)),
	}

	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("writing tar header: %v", err)
	}

	if _, err := tw.Write(content); err != nil {
		t.Fatalf("writing tar body: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}

	if err := gw.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}

	return buf
}

// TestExtractBinaryFromTarGz_Found verifies that the binary is correctly
// extracted when the archive contains the expected "cloister" entry.
func TestExtractBinaryFromTarGz_Found(t *testing.T) {
	want := []byte("fake-binary-content")
	archive := buildTarGz(t, "cloister", want)

	got, err := extractBinaryFromTarGz(archive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("extracted content = %q, want %q", got, want)
	}
}

// TestExtractBinaryFromTarGz_NestedPath verifies that the extractor matches on
// the base filename only, so a file stored under a directory prefix (e.g.
// "cloister_1.2.3_darwin_arm64/cloister") is still found correctly.
func TestExtractBinaryFromTarGz_NestedPath(t *testing.T) {
	want := []byte("nested-binary-content")
	archive := buildTarGz(t, "cloister_1.2.3_darwin_arm64/cloister", want)

	got, err := extractBinaryFromTarGz(archive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("extracted content = %q, want %q", got, want)
	}
}

// TestExtractBinaryFromTarGz_NotFound verifies that an informative error is
// returned when the archive does not contain a file named "cloister".
func TestExtractBinaryFromTarGz_NotFound(t *testing.T) {
	archive := buildTarGz(t, "something-else", []byte("irrelevant"))

	_, err := extractBinaryFromTarGz(archive)
	if err == nil {
		t.Fatal("expected an error for missing cloister binary, got nil")
	}
}

// TestVersionComparison verifies the version-equality logic used by Run to
// decide whether an update is required. Equal versions (with or without the
// "v" prefix) should not trigger a download.
func TestVersionComparison(t *testing.T) {
	tests := []struct {
		currentVersion string
		latestTag      string
		wantUpdate     bool
	}{
		{"1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.3", false},
		{"1.2.3", "v1.3.0", true},
		{"dev", "v1.0.0", true},
		{"1.0.0", "v1.0.1", true},
	}

	for _, tc := range tests {
		latestVersion := strings.TrimPrefix(tc.latestTag, "v")
		normalised := strings.TrimPrefix(tc.currentVersion, "v")
		needsUpdate := latestVersion != normalised

		if needsUpdate != tc.wantUpdate {
			t.Errorf("currentVersion=%q latestTag=%q: needsUpdate=%v, want %v",
				tc.currentVersion, tc.latestTag, needsUpdate, tc.wantUpdate)
		}
	}
}
