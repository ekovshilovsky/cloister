// Proprietary and confidential. All rights reserved.

package vcsbroker

import (
	"fmt"
	"strconv"
	"strings"

	"cloister.io/internal/vm"
)

// DeployGuest installs static git and gh shims plus the current tunnel token.
func DeployGuest(backend vm.Backend, profile string, guestPort int, token string) error {
	if guestPort <= 0 || guestPort > 65535 || token == "" || strings.ContainsAny(token, "'\r\n") {
		return fmt.Errorf("invalid guest VCS broker configuration")
	}
	script := guestInstallScript + "\ncat > \"$HOME/.cloister/vcs-broker.env\" <<'CLOISTER_VCS_ENV'\n" +
		"CLOISTER_VCS_URL='http://127.0.0.1:" + strconv.Itoa(guestPort) + "/v1/exec'\n" +
		"CLOISTER_VCS_TOKEN='" + token + "'\n" +
		"CLOISTER_VCS_ENV\nchmod 0600 \"$HOME/.cloister/vcs-broker.env\"\n"
	if _, err := backend.SSHScript(profile, script); err != nil {
		return fmt.Errorf("deploying guest VCS shims: %w", err)
	}
	return nil
}

// RemoveGuestConfig makes a stopped service fail as unavailable immediately.
func RemoveGuestConfig(backend vm.Backend, profile string) {
	_, _ = backend.SSHCommand(profile, `rm -f "$HOME/.cloister/vcs-broker.env"`)
}

const guestInstallScript = `set -eu
mkdir -p "$HOME/.cloister/bin" "$HOME/.cloister/lib" "$HOME/.local/bin"
for tool in git gh; do
    shim="$HOME/.local/bin/$tool"
    real_file="$HOME/.cloister/bin/$tool.real-path"
    if [ ! -f "$real_file" ]; then
        real="$(command -v "$tool" 2>/dev/null || true)"
        if [ "$real" != "$shim" ]; then
            printf '%s\n' "$real" > "$real_file"
            chmod 0600 "$real_file"
        fi
    fi
done
cat > "$HOME/.cloister/lib/vcs-shim" <<'CLOISTER_VCS_SHIM'
#!/usr/bin/env bash
set -u
tool="$(basename "$0")"
cwd="$(pwd -P)"
config="$HOME/.cloister/vcs-broker.env"
real_file="$HOME/.cloister/bin/$tool.real-path"

outside_mapped=true
case "$cwd/" in
    "$HOME/workspaces/"*) outside_mapped=false ;;
esac

if [[ ! -r "$config" ]]; then
    if $outside_mapped && [[ -r "$real_file" ]]; then
        real="$(<"$real_file")"
        if [[ -n "$real" && -x "$real" ]]; then exec "$real" "$@"; fi
    fi
    echo "cloister: VCS broker is unavailable for synchronized workspace $cwd" >&2
    exit 125
fi

if $outside_mapped; then
    if [[ -r "$real_file" ]]; then
        real="$(<"$real_file")"
        if [[ -n "$real" && -x "$real" ]]; then exec "$real" "$@"; fi
    fi
    echo "cloister: real guest $tool is unavailable outside a synchronized workspace" >&2
    exit 127
fi

source "$config"
headers="$(mktemp)"
trap 'rm -f "$headers"' EXIT
curl_args=(--http1.1 --fail --silent --show-error --no-buffer -D "$headers"
    -H "Authorization: Bearer $CLOISTER_VCS_TOKEN"
    --data-urlencode "tool=$tool" --data-urlencode "cwd=$cwd")
for arg in "$@"; do curl_args+=(--data-urlencode "arg=$arg"); done
if [[ ${GIT_EDITOR+x} ]]; then curl_args+=(--data-urlencode "env=GIT_EDITOR=$GIT_EDITOR"); fi
if [[ ${GIT_SEQUENCE_EDITOR+x} ]]; then curl_args+=(--data-urlencode "env=GIT_SEQUENCE_EDITOR=$GIT_SEQUENCE_EDITOR"); fi
if [[ ${GIT_TERMINAL_PROMPT+x} ]]; then curl_args+=(--data-urlencode "env=GIT_TERMINAL_PROMPT=$GIT_TERMINAL_PROMPT"); fi
curl "${curl_args[@]}" "$CLOISTER_VCS_URL"
curl_status=$?
if [[ $curl_status -ne 0 ]]; then exit 125; fi
exit_code="$(awk 'tolower($1)=="x-cloister-exit-code:" {gsub("\\r", "", $2); code=$2} END {print code}' "$headers")"
if [[ ! "$exit_code" =~ ^[0-9]+$ || "$exit_code" -gt 255 ]]; then
    echo "cloister: VCS broker response omitted a valid exit code" >&2
    exit 125
fi
exit "$exit_code"
CLOISTER_VCS_SHIM
chmod 0755 "$HOME/.cloister/lib/vcs-shim"
ln -sfn "$HOME/.cloister/lib/vcs-shim" "$HOME/.local/bin/git"
ln -sfn "$HOME/.cloister/lib/vcs-shim" "$HOME/.local/bin/gh"`
