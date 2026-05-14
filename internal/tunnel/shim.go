package tunnel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/vm"
)

// DeployShims deploys authentication tokens and configuration for tunneled
// services into the VM. The shim binaries and supporting packages are
// installed during base provisioning via APT/Homebrew; this function handles
// the host-side token deployment that cannot be done from inside the VM and
// must be re-run on every entry so a host-side token rotation (cc-clip
// restart, op-forward re-install, etc.) propagates to the running VM without
// a full re-provision.
func DeployShims(profile string, backend vm.Backend, available []DiscoveryResult) error {
	for _, r := range available {
		if !r.Available || r.Blocked {
			continue
		}
		switch r.Tunnel.Name {
		case "op-forward":
			if err := deployOpForwardToken(profile, backend); err != nil {
				// Token deployment failure is non-fatal — the user can
				// still enter the VM and deploy the token manually.
				fmt.Printf("  Warning: op-forward token deployment: %v\n", err)
			}
		case "clipboard":
			if err := deployClipboardToken(profile, backend); err != nil {
				fmt.Printf("  Warning: clipboard token deployment: %v\n", err)
			}
		}
	}
	return nil
}

// deployOpForwardToken copies the op-forward refresh token from the host
// into the VM and, when a host-side access token is available, exercises the
// end-to-end auth chain by hitting /op/execute with a benign --version call.
// The probe distinguishes three outcomes:
//
//   - access token present and accepted (200): full chain verified, ✓ logged
//     with explicit "authenticated" wording.
//   - access token present but expired (401): expected for short-lived access
//     tokens; the in-VM shim's refresh flow will mint a new pair on first
//     use, so the deploy still succeeds with an informational note.
//   - access token unreadable or daemon unreachable: refresh deploy still
//     succeeds (the shim's refresh path is the load-bearing one); the probe
//     outcome is reported softly so the user has a signal but is not blocked.
//
// We deliberately do not call /token/refresh as the verification probe:
// op-forward's daemon rotates the refresh token on every successful refresh
// call (server.go:186), so using it for liveness checking would mutate state
// on every cloister enter. /op/execute is read-only with respect to tokens.
func deployOpForwardToken(profile string, backend vm.Backend) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	refreshPath := filepath.Join(home, "Library", "Caches", "op-forward", "refresh.token")
	refreshRaw, err := os.ReadFile(refreshPath)
	if err != nil {
		return fmt.Errorf("reading op-forward refresh token at %s: %w", refreshPath, err)
	}

	script := fmt.Sprintf(
		"mkdir -p ~/.cache/op-forward && echo '%s' > ~/.cache/op-forward/refresh.token && chmod 600 ~/.cache/op-forward/refresh.token",
		string(refreshRaw),
	)
	if _, err := backend.SSHCommand(profile, script); err != nil {
		return fmt.Errorf("writing op-forward refresh token to VM: %w", err)
	}

	// Probe the daemon with the host's current access token; the result is
	// informational only, never blocks the deploy. See the function comment
	// for the three-outcome rationale.
	accessPath := filepath.Join(home, "Library", "Caches", "op-forward", "access.token")
	accessRaw, accessErr := os.ReadFile(accessPath)
	switch {
	case accessErr != nil:
		fmt.Println("  ✓ op-forward refresh token deployed (no host access token to verify; first use will refresh)")
	case verifyOpForwardAuth(extractBearerToken(accessRaw)):
		fmt.Println("  ✓ op-forward refresh token deployed and access chain authenticated")
	default:
		fmt.Println("  ✓ op-forward refresh token deployed (host access token expired or daemon unreachable; first use will refresh)")
	}

	return nil
}

// verifyOpForwardAuth issues a POST /op/execute with a no-op --version
// payload using the supplied access token as bearer. A 200 confirms the
// daemon, the access token, and the underlying op CLI are all healthy; any
// other status (notably 401 for an expired access token) is treated as
// "not currently verified" so the caller can fall back to a softer success
// message instead of marking the deploy as failed.
//
// --version is the safest payload for a liveness probe: it does not touch
// 1Password's biometric prompt, does not require an unlocked vault, and
// produces deterministic output. The 3-second timeout matches the
// dialTimeout used elsewhere in this package so probe latency is bounded
// even when the host daemon is stuck.
func verifyOpForwardAuth(accessToken string) bool {
	if accessToken == "" {
		return false
	}
	body, err := json.Marshal(map[string]any{"args": []string{"--version"}})
	if err != nil {
		return false
	}
	req, err := http.NewRequest("POST", "http://127.0.0.1:18340/op/execute", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// deployClipboardToken copies the cc-clip session token from the host into the
// VM and verifies the host token actually authenticates against the running
// daemon before declaring success. The verification step is what makes this a
// real fix rather than an open-loop copy: the cc-clip launchd service can
// rotate its token at any time (cold restart, `cc-clip service install`,
// explicit `--rotate-token`), and a stale host file would otherwise be
// silently propagated to the VM where every paste then 401s. When the
// authenticated probe fails the function returns a descriptive error without
// touching the VM, so a stale token cannot replace an already-good VM token.
func deployClipboardToken(profile string, backend vm.Backend) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	tokenPath := filepath.Join(home, ".cache", "cc-clip", "session.token")
	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		return fmt.Errorf("reading cc-clip token at %s: %w", tokenPath, err)
	}

	bareToken := extractBearerToken(raw)
	if bareToken == "" {
		return fmt.Errorf("cc-clip token file at %s is empty or malformed", tokenPath)
	}

	// Verify the host token currently authenticates against the daemon. If
	// the daemon has rotated to a new token the host file may be stale; in
	// that case propagating it to the VM would replace a working token with
	// a broken one. Failing loudly here lets the user trigger a refresh
	// (e.g. `cc-clip service uninstall && cc-clip service install`) before
	// retrying instead of chasing a silent 401 from Claude Code.
	if !verifyClipboardAuth(bareToken) {
		return fmt.Errorf("host token failed authenticated probe against cc-clip on 127.0.0.1:18339; daemon may have rotated, try `cc-clip service uninstall && cc-clip service install`")
	}

	// printf %s ensures the literal token bytes are written without shell
	// interpolation of any embedded backslashes; chmod 600 matches the host
	// file's permissions so the VM-side copy is not world-readable.
	script := fmt.Sprintf(
		"mkdir -p ~/.cache/cc-clip && printf '%%s\\n' '%s' > ~/.cache/cc-clip/session.token && chmod 600 ~/.cache/cc-clip/session.token",
		bareToken,
	)
	if _, err := backend.SSHCommand(profile, script); err != nil {
		return fmt.Errorf("writing clipboard token into VM: %w", err)
	}

	fmt.Println("  ✓ clipboard token deployed and authenticated")
	return nil
}

// extractBearerToken returns the bearer token from a cloister-managed token
// file. Both cc-clip and op-forward use the same convention: line 1 holds the
// opaque token bytes, line 2 (when present) holds a human-readable expiry
// timestamp that the shims and daemons ignore. Surrounding whitespace is
// trimmed so a trailing newline, BOM, or CRLF from a hand-edited file does
// not silently corrupt the Authorization header.
func extractBearerToken(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	return strings.SplitN(trimmed, "\n", 2)[0]
}

// verifyClipboardAuth probes the cc-clip daemon's /clipboard/type endpoint
// with the supplied bearer token. The endpoint requires authentication, so a
// 200 response means both that the daemon is reachable and that the token is
// currently accepted. A 401 is the canonical signal that the token has
// rotated; any other non-401 status is treated as auth-OK because the daemon
// may legitimately return 204/404/etc. when the clipboard is empty or holds
// an unsupported type, and refusing to deploy in those cases would be wrong.
func verifyClipboardAuth(token string) bool {
	req, err := http.NewRequest("GET", "http://127.0.0.1:18339/clipboard/type", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "cc-clip/cloister")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusUnauthorized
}
