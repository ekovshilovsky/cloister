package vcsbroker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	linuxprovision "cloister.io/internal/provision/linux"
	"cloister.io/internal/vm"
)

// GuestControlTimeout bounds the SSH side of broker ensure and teardown.
const GuestControlTimeout = 12 * time.Second

// DeployGuest installs static git and gh shims plus the service-owned token.
func DeployGuest(backend vm.Backend, profile string, guestPort int, token, ownerID string) error {
	if guestPort <= 0 || guestPort > 65535 || !safeValue(token) || !safeValue(ownerID) {
		return fmt.Errorf("invalid guest VCS broker configuration")
	}
	content :=
		"CLOISTER_VCS_URL='http://127.0.0.1:" + strconv.Itoa(guestPort) + "/v1/exec'\n" +
			"CLOISTER_VCS_TOKEN='" + token + "'\n" +
			"CLOISTER_VCS_OWNER='" + ownerID + "'"
	script := guestInstallScript + "\n" + linuxprovision.AtomicGuestWriteScript("~/.cloister/vcs-broker.env", content)
	ctx, cancel := context.WithTimeout(context.Background(), GuestControlTimeout)
	defer cancel()
	if _, err := vm.SSHScriptContext(ctx, backend, profile, script); err != nil {
		return fmt.Errorf("deploying guest VCS shims: %w", err)
	}
	return nil
}

func safeValue(value string) bool {
	return value != "" && !strings.ContainsAny(value, "'\r\n")
}

// RemoveGuestConfig removes only the configuration written by ownerID.
func RemoveGuestConfig(backend vm.Backend, profile, ownerID string) {
	if !safeValue(ownerID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), GuestControlTimeout)
	defer cancel()
	_, _ = vm.SSHScriptContext(ctx, backend, profile, removeGuestConfigScript(ownerID))
}

func removeGuestConfigScript(ownerID string) string {
	return `config="$HOME/.cloister/vcs-broker.env"
if [ -f "$config" ] && grep -Fqx "CLOISTER_VCS_OWNER='` + ownerID + `'" "$config"; then
    rm -f -- "$config"
fi`
}

// ProbeGuest validates the service-owned guest configuration, then uses its
// token to authenticate through the actual guest endpoint. This covers the
// guest config, reverse tunnel, host listener, and broker handler.
func ProbeGuest(backend vm.Backend, profile string, guestPort int, token, ownerID string) bool {
	if guestPort <= 0 || guestPort > 65535 || !safeValue(token) || !safeValue(ownerID) {
		return false
	}
	url := "http://127.0.0.1:" + strconv.Itoa(guestPort)
	script := `config="$HOME/.cloister/vcs-broker.env"
[ -r "$config" ] || exit 1
. "$config"
[ "$CLOISTER_VCS_OWNER" = '` + ownerID + `' ] || exit 1
[ "$CLOISTER_VCS_TOKEN" = '` + token + `' ] || exit 1
[ "$CLOISTER_VCS_URL" = '` + url + `/v1/exec' ] || exit 1
status="$(curl --http1.1 --silent --show-error --max-time 2 --output /dev/null --write-out '%{http_code}' -H "Authorization: Bearer $CLOISTER_VCS_TOKEN" '` + url + `/v1/health')" || exit $?
printf '__CLVCS[%s]CLVCS__' "$status"`
	ctx, cancel := context.WithTimeout(context.Background(), GuestControlTimeout)
	defer cancel()
	out, err := vm.SSHCaptureContext(ctx, backend, profile, script)
	return err == nil && strings.Contains(out, "__CLVCS[204]CLVCS__")
}

const guestInstallPrelude = `set -eu
mkdir -p "$HOME/.cloister/bin" "$HOME/.cloister/lib" "$HOME/.local/bin"
for tool in git gh; do
    shim="$HOME/.local/bin/$tool"
    real_file="$HOME/.cloister/bin/$tool.real-path"
    if [ ! -f "$real_file" ]; then
        real="$(command -v "$tool" 2>/dev/null || true)"
        if [ "$real" != "$shim" ]; then
            real_tmp="$(mktemp "$HOME/.cloister/bin/.real-path.XXXXXX")"
            printf '%s\n' "$real" > "$real_tmp"
            chmod 0600 "$real_tmp"
            mv -fT -- "$real_tmp" "$real_file"
        fi
    fi
done`

const guestShimScript = `#!/usr/bin/env bash
set -u
tool="$(basename "$0")"
cwd="$(pwd -P)"
config="$HOME/.cloister/vcs-broker.env"
real_file="$HOME/.cloister/bin/$tool.real-path"

outside_mapped=true
case "$cwd/" in
    "$HOME/workspaces/"*) outside_mapped=false ;;
esac

run_real() {
    real=""
    if [[ -r "$real_file" ]]; then
        real="$(<"$real_file")"
        if [[ -n "$real" && -x "$real" ]]; then exec "$real" "$@"; fi
    fi
    # Broker shims can predate a base provision that installs gh. Preserve any
    # non-empty recorded path, but let a missing/empty legacy record discover
    # the base-owned system binary outside synchronized workspaces.
    if [[ -z "$real" && -x "/usr/bin/$tool" ]]; then
        exec "/usr/bin/$tool" "$@"
    fi
    return 1
}

if [[ ! -r "$config" ]]; then
    if $outside_mapped; then run_real "$@"; fi
    echo "cloister: VCS broker is unavailable for synchronized workspace $cwd" >&2
    exit 125
fi

if $outside_mapped; then
    run_real "$@"
    echo "cloister: real guest $tool is unavailable outside a synchronized workspace" >&2
    exit 127
fi

source "$config"
headers="$(mktemp)"
curl_error="$(mktemp)"
trap 'rm -f "$headers" "$curl_error"' EXIT
curl_args=(--http1.1 --silent --show-error --fail --no-buffer -D "$headers"
    -H "Authorization: Bearer $CLOISTER_VCS_TOKEN"
    --data-urlencode "tool=$tool" --data-urlencode "cwd=$cwd")
for arg in "$@"; do curl_args+=(--data-urlencode "arg=$arg"); done
if [[ ${GIT_EDITOR+x} ]]; then curl_args+=(--data-urlencode "env=GIT_EDITOR=$GIT_EDITOR"); fi
if [[ ${GIT_SEQUENCE_EDITOR+x} ]]; then curl_args+=(--data-urlencode "env=GIT_SEQUENCE_EDITOR=$GIT_SEQUENCE_EDITOR"); fi
if [[ ${GIT_TERMINAL_PROMPT+x} ]]; then curl_args+=(--data-urlencode "env=GIT_TERMINAL_PROMPT=$GIT_TERMINAL_PROMPT"); fi
if [[ ${GH_REPO+x} ]]; then curl_args+=(--data-urlencode "env=GH_REPO=$GH_REPO"); fi
# Admission closes for at most ten minutes while an accepted command drains,
# plus up to twelve seconds for guest refresh. The remaining three seconds
# cover scheduling jitter so a pre-execution 503 is normally invisible.
retry_deadline=$((SECONDS + 615))
retry_delay=1
while true; do
    : > "$headers"
    : > "$curl_error"
    curl "${curl_args[@]}" "$CLOISTER_VCS_URL" 2>"$curl_error"
    curl_status=$?
    http_status="$(awk 'toupper($1) ~ /^HTTP\// {status=$2} END {print status}' "$headers")"
    if [[ "$http_status" != "503" ]]; then break; fi
    if (( SECONDS >= retry_deadline )); then
        echo "cloister: VCS broker is restarting; retry command" >&2
        exit 75
    fi
    retry_after="$(awk 'tolower($1)=="retry-after:" {gsub("\\r", "", $2); value=$2} END {print value}' "$headers")"
    if [[ "$retry_after" =~ ^[1-9][0-9]*$ ]] && (( retry_after <= 5 )); then
        sleep_for=$retry_after
    else
        sleep_for=$retry_delay
    fi
    remaining=$((retry_deadline - SECONDS))
    if (( sleep_for > remaining )); then sleep_for=$remaining; fi
    sleep "$sleep_for"
    if (( retry_delay < 5 )); then retry_delay=$((retry_delay * 2)); fi
    if (( retry_delay > 5 )); then retry_delay=5; fi
done
if [[ $curl_status -ne 0 ]]; then
    cat "$curl_error" >&2
    exit 125
fi
if [[ ! "$http_status" =~ ^[0-9]+$ || "$http_status" -ge 400 ]]; then exit 125; fi
exit_code="$(awk 'tolower($1)=="x-cloister-exit-code:" {gsub("\\r", "", $2); code=$2} END {print code}' "$headers")"
if [[ ! "$exit_code" =~ ^[0-9]+$ || "$exit_code" -gt 255 ]]; then
    echo "cloister: VCS broker response omitted a valid exit code" >&2
    exit 125
fi
exit "$exit_code"`

const guestInstallLinks = `chmod 0755 "$HOME/.cloister/lib/vcs-shim"
ln -sfn "$HOME/.cloister/lib/vcs-shim" "$HOME/.local/bin/git"
ln -sfn "$HOME/.cloister/lib/vcs-shim" "$HOME/.local/bin/gh"`

func guestInstallScriptForShim(shim string) string {
	return guestInstallPrelude + "\n" +
		linuxprovision.AtomicGuestWriteScript("~/.cloister/lib/vcs-shim", shim) + "\n" +
		guestInstallLinks
}

var guestInstallScript = guestInstallScriptForShim(guestShimScript)
