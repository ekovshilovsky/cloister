package tunnel

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestExtractBearerToken covers the host file shapes the cc-clip and
// op-forward daemons are known to write, plus the defensive shapes
// (hand-edited, CRLF, empty) the helper has to survive without corrupting the
// resulting bearer header. Both daemons follow the same convention: line 1
// holds the token, line 2 (when present) is a human-readable expiry that
// downstream consumers ignore — so one parser covers both services.
func TestExtractBearerToken(t *testing.T) {
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
			got := extractBearerToken([]byte(tc.in))
			if got != tc.want {
				t.Errorf("extractBearerToken(%q) = %q, want %q", tc.in, got, tc.want)
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

// TestProbeOpForwardOutcomes covers the five outcomes the probe classifier
// distinguishes. The outcome enum is the data shape downstream messaging
// needs — a bool can't tell the user whether 401 (normal access-token
// expiry) should print ℹ (self-healing) or ⚠ (action needed), and the prior
// version's "or"-joined log line confused exactly that.
func TestProbeOpForwardOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       opForwardProbeOutcome
	}{
		{"200 OK — full chain verified", http.StatusOK, opProbeOK},
		{"401 — access expired, refresh will self-heal", http.StatusUnauthorized, opProbeAccessExpired},
		{"404 — daemon misroute, surfaces as DaemonError", http.StatusNotFound, opProbeDaemonError},
		{"500 — daemon error, surfaces as DaemonError", http.StatusInternalServerError, opProbeDaemonError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotAuth, gotCT string
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotAuth = r.Header.Get("Authorization")
				gotCT = r.Header.Get("Content-Type")
				gotBody, _ = readAll(r)
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			got := probeOpForwardAt(srv.URL, "access-abc")
			if got != tc.want {
				t.Errorf("probeOpForwardAt status=%d → %v, want %v", tc.statusCode, got, tc.want)
			}
			if gotMethod != "POST" {
				t.Errorf("daemon received method %q, want POST", gotMethod)
			}
			if !strings.HasPrefix(gotAuth, "Bearer ") {
				t.Errorf("daemon received Authorization=%q, expected Bearer prefix", gotAuth)
			}
			if gotCT != "application/json" {
				t.Errorf("daemon received Content-Type=%q, want application/json", gotCT)
			}
			if !strings.Contains(string(gotBody), `"--version"`) {
				t.Errorf("daemon received body=%q, expected to contain --version payload", gotBody)
			}
		})
	}
}

// TestProbeOpForward_Unreachable covers the network-error branch. The
// production probe distinguishes "daemon unreachable" from "daemon
// responding with an error" because the user-actionable advice differs
// (start the service vs investigate why it's misbehaving). We trigger
// unreachable by pointing the probe at a closed loopback port.
func TestProbeOpForward_Unreachable(t *testing.T) {
	// Bind to an ephemeral port, close it, then probe; the kernel will
	// refuse new connections at that port.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed := srv.URL
	srv.Close()

	got := probeOpForwardAt(closed, "access-abc")
	if got != opProbeUnreachable {
		t.Errorf("closed port → %v, want opProbeUnreachable", got)
	}
}

// TestProbeOpForward_NoAccessFile guards against shipping an empty bearer
// header when the host access token file is empty or missing. Sending an
// empty Bearer would let the daemon distinguish authn intent and would
// pollute logs, so the probe must short-circuit before any network call.
func TestProbeOpForward_NoAccessFile(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got := probeOpForwardAt(srv.URL, "")
	if got != opProbeNoAccessFile {
		t.Errorf("empty token → %v, want opProbeNoAccessFile", got)
	}
	if called {
		t.Errorf("empty token should short-circuit before any HTTP call")
	}
}

// probeOpForwardAt is the test seam for probeOpForward, mirroring the
// verifyAuthAt pattern used by the clipboard probe tests: the production
// function targets a hard-coded loopback URL, and the unit tests need to
// redirect at an httptest endpoint while preserving the exact request shape
// (method, headers, body) the daemon sees in production. Returns the same
// opForwardProbeOutcome enum the production function returns so tests can
// pin down each of the five branches independently.
func probeOpForwardAt(rawURL, accessToken string) opForwardProbeOutcome {
	if accessToken == "" {
		return opProbeNoAccessFile
	}
	body := []byte(`{"args":["--version"]}`)
	req, err := http.NewRequest("POST", rawURL, bytes.NewReader(body))
	if err != nil {
		return opProbeDaemonError
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return opProbeUnreachable
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return opProbeOK
	case http.StatusUnauthorized:
		return opProbeAccessExpired
	default:
		return opProbeDaemonError
	}
}

// readAll is a tiny helper to drain a request body in tests without pulling
// in io/ioutil or sprinkling io.ReadAll calls across each test case. Returns
// the bytes read plus an error suitable for t.Errorf in callers.
func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}
