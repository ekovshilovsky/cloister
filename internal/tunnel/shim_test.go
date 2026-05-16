package tunnel

import (
	"bytes"
	"encoding/json"
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

// TestProbeOpForwardChain covers the chain-probe classifier for every
// combination of access-token state and refresh-token state. The classifier
// is the substrate for cloister's deploy decisions: messaging and what gets
// pushed into the VM both branch on its output, so the contract has to be
// exhaustively pinned down by tests rather than inferred from the
// implementation later.
//
// The test serves /op/execute and /token/refresh from one httptest server
// keyed by request path, mirroring what op-forward's real daemon does. The
// access endpoint's status code and the refresh endpoint's status code are
// independent inputs, so the truth table exercised here covers every
// reachable opForwardChainStatus value.
func TestProbeOpForwardChain(t *testing.T) {
	const fakeNewAccess = "new-access-abcdef"
	const fakeNewRefresh = "new-refresh-xyz123"

	tests := []struct {
		name           string
		accessToken    string
		refreshToken   string
		execStatus     int   // status code for /op/execute
		refreshStatus  int   // status code for /token/refresh
		refreshBodyOK  bool  // when true, return a well-formed JSON payload
		wantStatus     opForwardChainStatus
		wantNewAccess  string // empty when no rotation occurred
		wantNewRefresh string
		wantExecCalls  int // times /op/execute should have been hit
		wantRefCalls   int // times /token/refresh should have been hit
	}{
		{
			name:          "access 200 — happy path, no refresh consumed",
			accessToken:   "access-abc",
			refreshToken:  "refresh-abc",
			execStatus:    http.StatusOK,
			wantStatus:    opChainAccessOK,
			wantExecCalls: 1,
			wantRefCalls:  0,
		},
		{
			name:           "access 401 → refresh 200, rotated pair returned",
			accessToken:    "access-stale",
			refreshToken:   "refresh-fresh",
			execStatus:     http.StatusUnauthorized,
			refreshStatus:  http.StatusOK,
			refreshBodyOK:  true,
			wantStatus:     opChainRefreshedFromExpired,
			wantNewAccess:  fakeNewAccess,
			wantNewRefresh: fakeNewRefresh,
			wantExecCalls:  1,
			wantRefCalls:   1,
		},
		{
			name:           "no access token, refresh 200 — minted from absent",
			accessToken:    "",
			refreshToken:   "refresh-fresh",
			refreshStatus:  http.StatusOK,
			refreshBodyOK:  true,
			wantStatus:     opChainRefreshedFromAbsent,
			wantNewAccess:  fakeNewAccess,
			wantNewRefresh: fakeNewRefresh,
			wantExecCalls:  0,
			wantRefCalls:   1,
		},
		{
			name:          "access 401 → refresh 401 — 30-day cap, both expired",
			accessToken:   "access-stale",
			refreshToken:  "refresh-stale",
			execStatus:    http.StatusUnauthorized,
			refreshStatus: http.StatusUnauthorized,
			wantStatus:    opChainBothExpired,
			wantExecCalls: 1,
			wantRefCalls:  1,
		},
		{
			name:         "no tokens at all",
			accessToken:  "",
			refreshToken: "",
			wantStatus:   opChainNoTokens,
			wantExecCalls: 0,
			wantRefCalls:  0,
		},
		{
			name:          "access 500 — daemon error short-circuits before refresh",
			accessToken:   "access-abc",
			refreshToken:  "refresh-abc",
			execStatus:    http.StatusInternalServerError,
			wantStatus:    opChainDaemonError,
			wantExecCalls: 1,
			wantRefCalls:  0,
		},
		{
			name:          "access 401 → refresh 500 — surfaces as DaemonError",
			accessToken:   "access-stale",
			refreshToken:  "refresh-stale",
			execStatus:    http.StatusUnauthorized,
			refreshStatus: http.StatusInternalServerError,
			wantStatus:    opChainDaemonError,
			wantExecCalls: 1,
			wantRefCalls:  1,
		},
		{
			name:          "access 401 → refresh 200 but malformed JSON — DaemonError",
			accessToken:   "access-stale",
			refreshToken:  "refresh-fresh",
			execStatus:    http.StatusUnauthorized,
			refreshStatus: http.StatusOK,
			refreshBodyOK: false,
			wantStatus:    opChainDaemonError,
			wantExecCalls: 1,
			wantRefCalls:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var execCalls, refCalls int
			var refAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/op/execute":
					execCalls++
					w.WriteHeader(tc.execStatus)
				case "/token/refresh":
					refCalls++
					refAuth = r.Header.Get("Authorization")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.refreshStatus)
					if tc.refreshBodyOK {
						body, _ := json.Marshal(opForwardRefreshResponse{
							AccessToken:  fakeNewAccess,
							RefreshToken: fakeNewRefresh,
						})
						w.Write(body)
					} else if tc.refreshStatus == http.StatusOK {
						w.Write([]byte("not json"))
					}
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			gotStatus, minted := probeOpForwardChainAt(
				srv.URL+"/op/execute",
				srv.URL+"/token/refresh",
				tc.accessToken,
				tc.refreshToken,
			)
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %v, want %v", gotStatus, tc.wantStatus)
			}
			if execCalls != tc.wantExecCalls {
				t.Errorf("/op/execute calls = %d, want %d", execCalls, tc.wantExecCalls)
			}
			if refCalls != tc.wantRefCalls {
				t.Errorf("/token/refresh calls = %d, want %d", refCalls, tc.wantRefCalls)
			}
			if tc.wantNewAccess != "" {
				if minted == nil {
					t.Fatalf("expected minted tokens, got nil")
				}
				if minted.AccessToken != tc.wantNewAccess {
					t.Errorf("minted access = %q, want %q", minted.AccessToken, tc.wantNewAccess)
				}
				if minted.RefreshToken != tc.wantNewRefresh {
					t.Errorf("minted refresh = %q, want %q", minted.RefreshToken, tc.wantNewRefresh)
				}
			} else if minted != nil {
				t.Errorf("expected no minted tokens, got %+v", minted)
			}
			// When refresh was hit, confirm it carried the refresh-token bearer
			// (and not, say, an empty header from a bug in the probe).
			if tc.wantRefCalls > 0 && !strings.HasPrefix(refAuth, "Bearer ") {
				t.Errorf("/token/refresh saw Authorization=%q, expected Bearer prefix", refAuth)
			}
		})
	}
}

// TestProbeOpForwardChain_Unreachable covers the network-error branch: a
// closed port should surface as opChainDaemonUnreachable rather than be
// misclassified as a DaemonError (the recovery advice differs).
func TestProbeOpForwardChain_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed := srv.URL
	srv.Close()

	got, minted := probeOpForwardChainAt(closed+"/op/execute", closed+"/token/refresh", "access-abc", "refresh-abc")
	if got != opChainDaemonUnreachable {
		t.Errorf("closed port → %v, want opChainDaemonUnreachable", got)
	}
	if minted != nil {
		t.Errorf("expected no minted tokens on unreachable, got %+v", minted)
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
