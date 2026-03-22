package vmcli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OllamaModel represents a single model entry returned by the Ollama API.
type OllamaModel struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}

type ollamaResponse struct {
	Models []OllamaModel `json:"models"`
}

// FetchOllamaModels queries the local Ollama API endpoint and returns the list
// of installed models. The Ollama server is expected to be reachable via the
// macOS host tunnel forwarded to 127.0.0.1:11434 inside the VM.
func FetchOllamaModels() ([]OllamaModel, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:11434/api/tags")
	if err != nil {
		return nil, fmt.Errorf("ollama tunnel is not connected. Enter the profile from the host:\n  cloister <profile>")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading ollama response: %w", err)
	}

	return parseOllamaResponse(body)
}

// parseOllamaResponse deserializes the raw JSON response body from the Ollama
// /api/tags endpoint into a slice of OllamaModel values.
func parseOllamaResponse(body []byte) ([]OllamaModel, error) {
	var resp ollamaResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing ollama response: %w", err)
	}
	return resp.Models, nil
}

// FormatModelSize converts a byte count into a human-readable string using
// gigabyte or megabyte units depending on magnitude.
func FormatModelSize(bytes int64) string {
	gb := float64(bytes) / 1e9
	if gb >= 1 {
		return fmt.Sprintf("%.1f GB", gb)
	}
	mb := float64(bytes) / 1e6
	return fmt.Sprintf("%.0f MB", mb)
}
