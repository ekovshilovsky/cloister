package vcsbroker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	linuxprovision "cloister.io/internal/provision/linux"
	"cloister.io/internal/vm"
)

// GuestControlTimeout bounds the SSH side of broker ensure and teardown.
const GuestControlTimeout = 12 * time.Second

const guestServiceConfigPath = "~/.cloister/vcs-broker-service.env"

// DeployGuest installs static git and gh shims plus the service-owned token.
func DeployGuest(backend vm.Backend, profile string, guestPort int, token, ownerID string) error {
	if guestPort <= 0 || guestPort > 65535 || !safeValue(token) || !safeValue(ownerID) {
		return fmt.Errorf("invalid guest VCS broker configuration")
	}
	content :=
		"CLOISTER_VCS_URL='http://127.0.0.1:" + strconv.Itoa(guestPort) + "/v1/exec'\n" +
			"CLOISTER_VCS_TOKEN='" + token + "'\n" +
			"CLOISTER_VCS_OWNER='" + ownerID + "'"
	script := guestInstallScript + "\n" + linuxprovision.AtomicGuestWriteScript(guestServiceConfigPath, content) + `
rm -f -- "$HOME/.cloister/vcs-broker.env"`
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
	return `config="$HOME/.cloister/vcs-broker-service.env"
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
	script := `config="$HOME/.cloister/vcs-broker-service.env"
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

// GuestInstallationStatus describes the generation-owned files installed in
// the guest without exposing their contents.
type GuestInstallationStatus struct {
	Config string
	Shim   string
}

// Current reports whether both generation-owned guest files match the daemon.
func (s GuestInstallationStatus) Current() bool {
	return s.Config == "current" && s.Shim == "current"
}

// EnsureGuestInstallation verifies the generation-owned config and exact shim
// contents, then atomically restores both with the same token when necessary.
func EnsureGuestInstallation(backend vm.Backend, profile string, guestPort int, token, ownerID string) (GuestInstallationStatus, bool, error) {
	if guestPort <= 0 || guestPort > 65535 || !safeValue(token) || !safeValue(ownerID) {
		return GuestInstallationStatus{}, false, fmt.Errorf("invalid guest VCS broker configuration")
	}
	status, err := inspectGuestInstallation(backend, profile, guestPort, token, ownerID)
	if err != nil || status.Current() {
		return status, false, err
	}
	if err := DeployGuest(backend, profile, guestPort, token, ownerID); err != nil {
		return status, false, err
	}
	return status, true, nil
}

func inspectGuestInstallation(backend vm.Backend, profile string, guestPort int, token, ownerID string) (GuestInstallationStatus, error) {
	url := "http://127.0.0.1:" + strconv.Itoa(guestPort) + "/v1/exec"
	// AtomicGuestWriteScript preserves the writer's historical trailing newline.
	shimSum := sha256.Sum256([]byte(guestShimScript + "\n"))
	script := `config="$HOME/.cloister/vcs-broker-service.env"
shim="$HOME/.cloister/lib/vcs-shim"
config_state=missing
if [ -f "$config" ]; then
    config_state=mismatch
    if [ ! -L "$config" ] && grep -Fqx "CLOISTER_VCS_URL='` + url + `'" "$config" &&
        grep -Fqx "CLOISTER_VCS_TOKEN='` + token + `'" "$config" &&
        grep -Fqx "CLOISTER_VCS_OWNER='` + ownerID + `'" "$config"; then
        config_state=current
    fi
fi
shim_state=missing
if [ -f "$shim" ]; then
    shim_state=mismatch
    shim_sum="$(sha256sum "$shim" 2>/dev/null | awk '{print $1}')" || shim_sum=""
    if [ ! -L "$shim" ] && [ "$shim_sum" = '` + fmt.Sprintf("%x", shimSum) + `' ]; then
        shim_state=current
    fi
fi
printf '__CLVGUEST[config=%s;shim=%s]CLVGUEST__' "$config_state" "$shim_state"`
	ctx, cancel := context.WithTimeout(context.Background(), GuestControlTimeout)
	defer cancel()
	out, err := vm.SSHCaptureContext(ctx, backend, profile, script)
	if err != nil {
		return GuestInstallationStatus{}, fmt.Errorf("checking guest VCS installation: %w", err)
	}
	const prefix = "__CLVGUEST[config="
	start := strings.Index(out, prefix)
	end := strings.Index(out, "]CLVGUEST__")
	if start < 0 || end < start {
		return GuestInstallationStatus{}, fmt.Errorf("checking guest VCS installation: unexpected output")
	}
	fields := strings.Split(out[start+len(prefix):end], ";shim=")
	if len(fields) != 2 || !validGuestInstallationState(fields[0]) || !validGuestInstallationState(fields[1]) {
		return GuestInstallationStatus{}, fmt.Errorf("checking guest VCS installation: invalid status")
	}
	return GuestInstallationStatus{Config: fields[0], Shim: fields[1]}, nil
}

func validGuestInstallationState(state string) bool {
	return state == "current" || state == "missing" || state == "mismatch"
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

const guestShimTemplate = `#!/usr/bin/env bash
set -u
__CLOISTER_RETRY_CLOCK__
tool="$(basename "$0")"
cwd="$(pwd -P)"
config="$HOME/.cloister/vcs-broker-service.env"
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
# plus up to thirty seconds for replacement startup. Five additional seconds
# cover scheduling jitter so a pre-execution rejection is normally invisible.
retry_started="$(vcs_retry_now)"
retry_deadline=$((retry_started + __CLOISTER_RETRY_BUDGET__))
retry_delay=1
retry_announced=false
while true; do
    : > "$headers"
    : > "$curl_error"
    curl "${curl_args[@]}" "$CLOISTER_VCS_URL" 2>"$curl_error"
    curl_status=$?
    http_status="$(awk 'toupper($1) ~ /^HTTP\// {status=$2} END {print status}' "$headers")"
    retry_reason=""
    if [[ "$http_status" == "503" && $curl_status -eq 22 ]]; then
        retry_reason="the broker is transitioning before command admission"
    elif [[ $curl_status -eq 5 || $curl_status -eq 6 || $curl_status -eq 7 ]]; then
        retry_reason="the broker connection failed before the command was sent"
    fi
    if [[ -z "$retry_reason" ]]; then break; fi
    now="$(vcs_retry_now)"
    if (( now >= retry_deadline )); then
        cat "$curl_error" >&2
        echo "cloister: $retry_reason; retry budget exhausted, retry the command later" >&2
        exit 75
    fi
    if ! $retry_announced; then
        echo "cloister: $retry_reason; retrying for up to __CLOISTER_RETRY_BUDGET__ seconds" >&2
        retry_announced=true
    fi
    retry_after="$(awk 'tolower($1)=="retry-after:" {gsub("\\r", "", $2); value=$2} END {print value}' "$headers")"
    if [[ "$retry_after" =~ ^[1-9][0-9]*$ ]] && (( retry_after <= 5 )); then
        sleep_for=$retry_after
    else
        sleep_for=$retry_delay
    fi
    remaining=$((retry_deadline - now))
    if (( sleep_for > remaining )); then sleep_for=$remaining; fi
    vcs_retry_sleep "$sleep_for"
    if (( retry_delay < 5 )); then retry_delay=$((retry_delay * 2)); fi
    if (( retry_delay > 5 )); then retry_delay=5; fi
done
if [[ $curl_status -ne 0 ]]; then
    cat "$curl_error" >&2
    if [[ $curl_status -eq 22 && "$http_status" =~ ^[0-9]+$ && "$http_status" -ge 400 ]]; then
        echo "cloister: the VCS broker rejected the request with HTTP $http_status before command execution; the command did not run" >&2
        exit 125
    fi
    echo "cloister: the broker connection was lost after the request may have been sent; the command may have completed on the host. Inspect git status and git log before retrying." >&2
    exit 74
fi
if [[ ! "$http_status" =~ ^[0-9]+$ || "$http_status" -ge 400 ]]; then exit 125; fi
exit_code="$(awk 'tolower($1)=="x-cloister-exit-code:" {gsub("\\r", "", $2); code=$2} END {print code}' "$headers")"
if [[ ! "$exit_code" =~ ^[0-9]+$ || "$exit_code" -gt 255 ]]; then
    echo "cloister: VCS broker response omitted a valid exit code" >&2
    exit 125
fi
exit "$exit_code"`

const guestShimRetryBudget = 635

const guestShimClockFunctions = `vcs_retry_now() { printf '%s\n' "$SECONDS"; }
vcs_retry_sleep() { sleep "$1"; }`

func renderGuestShim(clockFunctions string, retryBudget int) string {
	shim := strings.Replace(guestShimTemplate, "__CLOISTER_RETRY_CLOCK__", clockFunctions, 1)
	return strings.ReplaceAll(shim, "__CLOISTER_RETRY_BUDGET__", strconv.Itoa(retryBudget))
}

var guestShimScript = renderGuestShim(guestShimClockFunctions, guestShimRetryBudget)

const guestInstallLinks = `chmod 0755 "$HOME/.cloister/lib/vcs-shim"
ln -sfn "$HOME/.cloister/lib/vcs-shim" "$HOME/.local/bin/git"
ln -sfn "$HOME/.cloister/lib/vcs-shim" "$HOME/.local/bin/gh"`

func guestInstallScriptForShim(shim string) string {
	return guestInstallPrelude + "\n" +
		linuxprovision.AtomicGuestWriteScript("~/.cloister/lib/vcs-shim", shim) + "\n" +
		guestInstallLinks
}

var guestInstallScript = guestInstallScriptForShim(guestShimScript)
