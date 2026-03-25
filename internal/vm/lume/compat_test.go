package lume

import "testing"

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

