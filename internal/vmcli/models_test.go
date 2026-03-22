package vmcli

import (
	"testing"
)

func TestParseOllamaResponse(t *testing.T) {
	body := `{"models":[
		{"name":"gemma3:27b","size":17000000000,"modified_at":"2026-03-18T00:00:00Z"},
		{"name":"qwen2.5-coder:7b","size":4700000000,"modified_at":"2026-03-21T00:00:00Z"}
	]}`

	models, err := parseOllamaResponse([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].Name != "gemma3:27b" {
		t.Errorf("first model = %q, want gemma3:27b", models[0].Name)
	}
}

func TestParseOllamaResponseEmpty(t *testing.T) {
	body := `{"models":[]}`
	models, err := parseOllamaResponse([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}
}

func TestParseOllamaResponseMalformed(t *testing.T) {
	_, err := parseOllamaResponse([]byte(`{invalid`))
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestFormatModelSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 MB"},
		{500_000_000, "500 MB"},
		{999_000_000, "999 MB"},
		{1_000_000_000, "1.0 GB"},
		{1_900_000_000, "1.9 GB"},
		{4_700_000_000, "4.7 GB"},
		{17_000_000_000, "17.0 GB"},
	}
	for _, tt := range tests {
		got := FormatModelSize(tt.bytes)
		if got != tt.want {
			t.Errorf("FormatModelSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
