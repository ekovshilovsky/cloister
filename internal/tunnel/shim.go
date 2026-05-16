package tunnel

import (
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

// deployOpForwardToken classifies the current op-forward auth state via
// probeOpForwardChain and then deploys whichever refresh token will
// actually be accepted by the daemon. Unlike a one-shot probe-then-deploy,
// this function actively recovers when the host access token has expired:
// it issues /token/refresh, the daemon mints and persists a new pair, and
// the freshly-minted refresh token is what gets pushed into the VM. The
// previous behavior — copying whatever was on disk and hoping the in-VM
// shim could refresh on first use — gave users a green checkmark even when
// the refresh token was also expired (30-day cap reached) and the very
// next op call would fail. probeOpForwardChain tells us authoritatively
// which case we are in, so the displayed status matches reality.
func deployOpForwardToken(profile string, backend vm.Backend) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	refreshPath := filepath.Join(home, "Library", "Caches", "op-forward", "refresh.token")
	accessPath := filepath.Join(home, "Library", "Caches", "op-forward", "access.token")

	refreshRaw, _ := os.ReadFile(refreshPath)
	accessRaw, _ := os.ReadFile(accessPath)
	refreshTok := extractBearerToken(refreshRaw)
	accessTok := extractBearerToken(accessRaw)

	status, minted := probeOpForwardChain(accessTok, refreshTok)

	switch status {
	case opChainAccessOK:
		// Access works. Deploy the existing refresh as-is.
		if err := pushOpForwardRefreshToVM(profile, backend, refreshTok); err != nil {
			return err
		}
		fmt.Println("  ✓ op-forward refresh token deployed")
		fmt.Println("  ✓ op-forward access chain authenticated (end-to-end check passed)")
		return nil

	case opChainRefreshedFromExpired:
		// Access was expired; refresh succeeded and minted a new pair.
		// Deploy the newly-minted refresh, not the now-rotated-out one.
		if err := pushOpForwardRefreshToVM(profile, backend, minted.RefreshToken); err != nil {
			return err
		}
		fmt.Println("  ✓ op-forward refresh token deployed (rotated)")
		fmt.Println("  ✓ op-forward access chain re-authenticated via refresh (new access+refresh pair minted)")
		return nil

	case opChainRefreshedFromAbsent:
		// No prior access token; refresh succeeded and seeded a fresh pair.
		if err := pushOpForwardRefreshToVM(profile, backend, minted.RefreshToken); err != nil {
			return err
		}
		fmt.Println("  ✓ op-forward refresh token deployed (rotated)")
		fmt.Println("  ✓ op-forward access chain authenticated via refresh (no prior access token; new pair minted)")
		return nil

	case opChainBothExpired:
		// 30-day refresh cap reached. Skip deploy entirely so a stale
		// refresh token does not replace a possibly-newer VM-side copy,
		// and surface the explicit recovery command. Returns nil because
		// op-forward unavailability shouldn't block the rest of the
		// enter flow (other tunnels can still work).
		fmt.Println("  ✗ op-forward deploy skipped: both access and refresh tokens are expired (30-day cap reached)")
		fmt.Println("    Recover with: op-forward service uninstall && op-forward service install")
		fmt.Println("    Then re-enter this profile to push the new tokens into the VM.")
		return nil

	case opChainNoTokens:
		fmt.Println("  ⚠ op-forward not deployed: no tokens on host — run `op-forward service install`")
		return nil

	case opChainDaemonUnreachable:
		// Daemon down. Push whatever we have so the VM stays primed for
		// when the daemon comes back; the ⚠ surfaces the underlying issue.
		if refreshTok != "" {
			if err := pushOpForwardRefreshToVM(profile, backend, refreshTok); err != nil {
				return err
			}
			fmt.Println("  ✓ op-forward refresh token deployed (existing)")
		}
		fmt.Println("  ⚠ op-forward daemon unreachable at 127.0.0.1:18340 — check `op-forward service status`")
		return nil

	case opChainDaemonError:
		if refreshTok != "" {
			if err := pushOpForwardRefreshToVM(profile, backend, refreshTok); err != nil {
				return err
			}
			fmt.Println("  ✓ op-forward refresh token deployed (existing)")
		}
		fmt.Println("  ⚠ op-forward daemon returned an unexpected status — check `op-forward service status`")
		return nil
	}

	return nil
}

// pushOpForwardRefreshToVM writes the supplied refresh token into the VM's
// ~/.cache/op-forward/refresh.token with 0600 perms. Extracted from the
// inline deploy logic so each branch of the chain-status switch can call
// it with whatever token is appropriate (the existing on-disk value when
// auth is healthy, or the freshly-minted value after a rotation).
func pushOpForwardRefreshToVM(profile string, backend vm.Backend, token string) error {
	if token == "" {
		return fmt.Errorf("refusing to deploy an empty op-forward refresh token")
	}
	script := fmt.Sprintf(
		"mkdir -p ~/.cache/op-forward && printf '%%s\\n' '%s' > ~/.cache/op-forward/refresh.token && chmod 600 ~/.cache/op-forward/refresh.token",
		token,
	)
	if _, err := backend.SSHCommand(profile, script); err != nil {
		return fmt.Errorf("writing op-forward refresh token to VM: %w", err)
	}
	return nil
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
