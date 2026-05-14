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

// deployOpForwardToken copies the op-forward refresh token from the host
// into the VM so that the op shim can authenticate with the host daemon.
func deployOpForwardToken(profile string, backend vm.Backend) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	tokenPath := filepath.Join(home, "Library", "Caches", "op-forward", "refresh.token")
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return fmt.Errorf("reading op-forward token at %s: %w", tokenPath, err)
	}

	// Create the token directory and write the token inside the VM
	script := fmt.Sprintf(
		"mkdir -p ~/.cache/op-forward && echo '%s' > ~/.cache/op-forward/refresh.token && chmod 600 ~/.cache/op-forward/refresh.token",
		string(token),
	)
	if _, err := backend.SSHCommand(profile, script); err != nil {
		return fmt.Errorf("writing token to VM: %w", err)
	}

	fmt.Println("  ✓ op-forward token deployed")
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

	bareToken := extractClipboardToken(raw)
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

// extractClipboardToken returns the bearer token from the cc-clip session.token
// file content. The host file uses a two-line format with the token on line 1
// and a human-readable expiry timestamp on line 2; the in-VM shim reads only
// line 1, and so does this helper. Surrounding whitespace is trimmed so a
// trailing newline, BOM, or CRLF from a hand-edited file does not silently
// corrupt the Authorization header.
func extractClipboardToken(raw []byte) string {
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
