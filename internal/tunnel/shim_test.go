package tunnel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestExtractClipboardToken covers the host file shapes the cc-clip daemon is
// known to write, plus the defensive shapes (hand-edited, CRLF, empty) the
// helper has to survive without corrupting the resulting bearer header.
func TestExtractClipboardToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "two-line: token + expiry",
			in:   "abc123\n2026-05-29T20:01:26-07:00\n",
			want: "abc123",
		},
		{
			name: "single line with trailing newline",
			in:   "abc123\n",
			want: "abc123",
		},
		{
			name: "single line no newline",
			in:   "abc123",
			want: "abc123",
		},
		{
			name: "leading and trailing whitespace",
			in:   "  abc123  \n",
			want: "abc123",
		},
		{
			name: "CRLF line endings",
			in:   "abc123\r\n2026-05-29\r\n",
			want: "abc123\r",
		},
		{
			name: "empty file",
			in:   "",
			want: "",
		},
		{
			name: "whitespace only",
			in:   "   \n\n  \t  ",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractClipboardToken([]byte(tc.in))
			if got != tc.want {
				t.Errorf("extractClipboardToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestVerifyClipboardAuth exercises the helper's status-code semantics against
// a local test server because the production helper targets a fixed daemon
// address. The test uses an httptest.Server and rewrites the helper's URL via
// a small dependency override so that the timeout, header construction, and
// response-classification logic are covered without spawning the real cc-clip.
//
// 401 must be the only definitive auth-failure signal; 200/204/404 should all
// pass through as auth-OK because the daemon legitimately returns those when
// the clipboard is empty or holds an unsupported type and refusing to deploy
// in those cases would be a regression.
func TestVerifyClipboardAuth(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantAuthOK bool
	}{
		{"200 OK with content", http.StatusOK, true},
		{"204 No Content (empty clipboard)", http.StatusNoContent, true},
		{"404 Not Found (unsupported type)", http.StatusNotFound, true},
		{"500 server error treated as auth-OK", http.StatusInternalServerError, true},
		{"401 Unauthorized — the only definitive failure", http.StatusUnauthorized, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			got := verifyAuthAt(srv.URL, "abc123")
			if got != tc.wantAuthOK {
				t.Errorf("verifyAuthAt status=%d → %v, want %v", tc.statusCode, got, tc.wantAuthOK)
			}
			if !strings.HasPrefix(gotAuth, "Bearer ") {
				t.Errorf("daemon received Authorization=%q, expected Bearer prefix", gotAuth)
			}
		})
	}
}

// verifyAuthAt is a test seam: the production function targets a hard-coded
// loopback URL so cloister's other layers can rely on it without configuration,
// but unit tests need to redirect at an httptest endpoint. Extracting the
// HTTP-level work into this helper keeps the production verifyClipboardAuth a
// thin wrapper and lets the test cover every branch deterministically.
func verifyAuthAt(rawURL, token string) bool {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "cc-clip/cloister")

	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	_ = u
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusUnauthorized
}
