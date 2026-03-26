package lume

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestParseIPSWVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "https://updates.cdn-apple.com/2026WinterFCS/fullrestores/122-00766/062A6121-2ABE-45D7-BCB1-72B666B6D2C2/UniversalMac_26.4_25E246_Restore.ipsw",
			want:  "26.4",
		},
		{
			input: "[INFO] Found latest IPSW URL url=https://example.com/UniversalMac_15.3_ABC123_Restore.ipsw\nhttps://example.com/UniversalMac_15.3_ABC123_Restore.ipsw",
			want:  "15.3",
		},
		{
			input: "some unrelated output",
			want:  "",
		},
		{
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		got := parseIPSWVersion(tt.input)
		if got != tt.want {
			label := tt.input
			if len(label) > 50 {
				label = label[:50]
			}
			t.Errorf("parseIPSWVersion(%q) = %q, want %q", label, got, tt.want)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		host     string
		required string
		want     bool
	}{
		{"26.4", "26.4", true},    // equal
		{"26.5", "26.4", true},    // host newer
		{"26.3", "26.4", false},   // host older
		{"27.0", "26.4", true},    // host major newer
		{"25.9", "26.4", false},   // host major older
		{"26.4.1", "26.4", true},  // host has more components
		{"26", "26.4", false},     // host has fewer components
	}

	for _, tt := range tests {
		got := versionAtLeast(tt.host, tt.required)
		if got != tt.want {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tt.host, tt.required, got, tt.want)
		}
	}
}

func TestIsIPSWCacheValid_WithChecksum(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "test.ipsw")
	checksumPath := localPath + ".sha256"

	content := []byte("fake ipsw content for testing")
	os.WriteFile(localPath, content, 0o600)

	// Compute correct checksum.
	h := sha256.Sum256(content)
	correctChecksum := hex.EncodeToString(h[:])
	os.WriteFile(checksumPath, []byte(correctChecksum), 0o600)

	// Valid checksum should pass.
	if !isIPSWCacheValid(localPath, checksumPath, "http://example.com/test.ipsw") {
		t.Error("expected cache to be valid with correct checksum")
	}

	// Wrong checksum should fail.
	os.WriteFile(checksumPath, []byte("wrong-checksum"), 0o600)
	if isIPSWCacheValid(localPath, checksumPath, "http://example.com/test.ipsw") {
		t.Error("expected cache to be invalid with wrong checksum")
	}
}

func TestIsIPSWCacheValid_MissingFile(t *testing.T) {
	if isIPSWCacheValid("/nonexistent/path.ipsw", "/nonexistent/path.sha256", "http://example.com") {
		t.Error("expected cache to be invalid for missing file")
	}
}

func TestIsIPSWCacheValid_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "empty.ipsw")
	os.WriteFile(localPath, []byte{}, 0o600)

	if isIPSWCacheValid(localPath, localPath+".sha256", "http://example.com") {
		t.Error("expected cache to be invalid for empty file")
	}
}

func TestReverseLines(t *testing.T) {
	got := reverseLines("a\nb\nc")
	want := []string{"c", "b", "a"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i, g := range got {
		if g != want[i] {
			t.Errorf("line %d: got %q, want %q", i, g, want[i])
		}
	}
}

