package tunnel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

// opForwardChainStatus describes the seven distinct end-states cloister
// reaches after actively probing the op-forward daemon. The classifier first
// tries the access token against /op/execute; on 401 (or when no access
// token is on the host) it then tries the refresh token against
// /token/refresh, which the daemon rotates on success. Treating these as
// separate values lets the caller print a single, factual message per
// outcome instead of speculating about what "first use" will do.
type opForwardChainStatus int

const (
	// opChainAccessOK: access token works (200 from /op/execute). The full
	// chain is verified without consuming a refresh rotation.
	opChainAccessOK opForwardChainStatus = iota

	// opChainRefreshedFromExpired: access was 401, refresh was 200 and
	// minted a new access/refresh pair which the daemon also persisted to
	// disk. The new tokens are returned to the caller so the in-VM shim
	// can be seeded with them in the same enter cycle.
	opChainRefreshedFromExpired

	// opChainRefreshedFromAbsent: no host access token was available, so
	// the classifier went straight to /token/refresh and received a new
	// pair. Functionally identical to RefreshedFromExpired but reported
	// with a distinct message so users understand why they didn't see a
	// "401 → mint" transition.
	opChainRefreshedFromAbsent

	// opChainBothExpired: both /op/execute (or absent access) and
	// /token/refresh returned 401. The 30-day refresh cap has been
	// reached and the user must reinstall the op-forward service to
	// mint fresh credentials. The deploy must be skipped — propagating
	// a stale refresh token to the VM only delays the same failure.
	opChainBothExpired

	// opChainNoTokens: neither token is on the host. The caller should
	// skip deploy entirely and direct the user to install op-forward.
	opChainNoTokens

	// opChainDaemonUnreachable: the HTTP request did not complete (the
	// daemon is down, the port is closed, etc.). The deploy proceeds
	// best-effort so the VM has whatever-was-on-disk for when the daemon
	// comes back, with a clear ⚠ surfaced about the underlying problem.
	opChainDaemonUnreachable

	// opChainDaemonError: the daemon answered but with an unexpected
	// status (5xx, 404 on the registered routes, malformed JSON, etc.).
	// Like Unreachable, the deploy proceeds best-effort with a ⚠.
	opChainDaemonError
)

// opForwardRefreshResponse mirrors the JSON shape returned by op-forward's
// /token/refresh endpoint (TokenRefreshResponse in op-forward source).
// Cloister only consumes the two value fields; expiry strings are
// informational on the daemon side and not consumed here.
type opForwardRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// probeOpForwardChain classifies the current op-forward authentication
// state in a single call. The probe is structured so callers can decide
// what to deploy and what to say without re-probing or speculating:
//
//   - if accessToken is non-empty, /op/execute is hit first. A 200 is
//     the cheap happy path and skips the refresh rotation entirely.
//   - on access 401 (or when accessToken is empty), /token/refresh is
//     hit with refreshToken. A 200 here mints (and on the daemon side
//     persists) a new access/refresh pair, which is returned to the
//     caller so the VM-side shim can be seeded in the same cycle.
//   - on refresh 401, the chain is fully expired and the caller is
//     expected to bail with a recovery hint.
//   - network errors and unexpected statuses are reported as their own
//     statuses so the caller can pick best-effort-deploy vs skip-deploy.
//
// Returning a *opForwardRefreshResponse alongside the status (nil when no
// rotation occurred) lets the caller obtain the freshly-minted tokens
// without re-reading the host files after the daemon persisted them — a
// race-free path that does not depend on filesystem timing.
func probeOpForwardChain(accessToken, refreshToken string) (opForwardChainStatus, *opForwardRefreshResponse) {
	return probeOpForwardChainAt(
		"http://127.0.0.1:18340/op/execute",
		"http://127.0.0.1:18340/token/refresh",
		accessToken,
		refreshToken,
	)
}

// probeOpForwardChainAt is the test seam for probeOpForwardChain: it
// accepts the two endpoint URLs as parameters so unit tests can redirect
// at httptest endpoints while preserving the request shape (method,
// headers, body) the daemon sees in production. The production wrapper
// passes the hard-coded loopback URLs.
func probeOpForwardChainAt(execURL, refreshURL, accessToken, refreshToken string) (opForwardChainStatus, *opForwardRefreshResponse) {
	if accessToken == "" && refreshToken == "" {
		return opChainNoTokens, nil
	}

	// Step 1: probe access (when we have one). 200 is the cheap happy
	// path; 401 falls through to refresh. Any other outcome short-
	// circuits to a daemon-state classification so we don't consume a
	// refresh rotation against a misbehaving daemon.
	if accessToken != "" {
		status, transport := probeAccess(execURL, accessToken)
		switch {
		case status == http.StatusOK:
			return opChainAccessOK, nil
		case transport != nil:
			return opChainDaemonUnreachable, nil
		case status == http.StatusUnauthorized:
			// fall through to refresh
		default:
			return opChainDaemonError, nil
		}
	}

	// Step 2: probe refresh. Empty refreshToken at this point means the
	// caller has only an access token (or no tokens at all), which we
	// can no longer recover from without manual intervention.
	if refreshToken == "" {
		return opChainNoTokens, nil
	}

	resp, body, status, transport := probeRefresh(refreshURL, refreshToken)
	switch {
	case transport != nil:
		return opChainDaemonUnreachable, nil
	case status == http.StatusOK:
		// On success the daemon returns both new tokens. Parse them so
		// the caller can deploy without re-reading the host file.
		var parsed opForwardRefreshResponse
		if err := json.Unmarshal(body, &parsed); err != nil || parsed.RefreshToken == "" {
			return opChainDaemonError, nil
		}
		_ = resp // body already read; resp.Body.Close handled by probeRefresh
		if accessToken == "" {
			return opChainRefreshedFromAbsent, &parsed
		}
		return opChainRefreshedFromExpired, &parsed
	case status == http.StatusUnauthorized:
		return opChainBothExpired, nil
	default:
		return opChainDaemonError, nil
	}
}

// probeAccess issues a POST /op/execute with a no-op --version payload
// authenticated by accessToken. Returns the HTTP status code, or a non-nil
// transport error when the request did not complete. --version is the
// safest payload: it does not touch 1Password's biometric prompt and does
// not require an unlocked vault.
func probeAccess(execURL, accessToken string) (int, error) {
	body, err := json.Marshal(map[string]any{"args": []string{"--version"}})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest("POST", execURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// probeRefresh issues a POST /token/refresh with refreshToken as bearer
// and returns the response (for body parsing), the body bytes, the HTTP
// status, and any transport error. The /token/refresh endpoint rotates
// the refresh token on success, so this function must only be called
// when the caller is prepared to commit to the new tokens. The body is
// returned separately because callers need to decode the JSON response
// body and the resp.Body has already been closed.
func probeRefresh(refreshURL, refreshToken string) (*http.Response, []byte, int, error) {
	req, err := http.NewRequest("POST", refreshURL, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+refreshToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return resp, nil, resp.StatusCode, err
	}
	return resp, buf.Bytes(), resp.StatusCode, nil
}
