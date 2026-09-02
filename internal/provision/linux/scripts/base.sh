#!/bin/bash
set -euo pipefail
echo "=== Installing base tools ==="

# apt sources sanity pre-flight. Older dotnet-stack versions and certain
# external Microsoft tooling .deb installers registered the
# packages.microsoft.com repo with a legacy signing-key path
# (/usr/share/keyrings/microsoft-prod.gpg). Current cloister-managed
# stack-dotnet.sh uses /etc/apt/keyrings/microsoft.gpg. If both entries
# coexist in /etc/apt/sources.list.d/, the apt-get update below fails with
# "Conflicting values set for option Signed-By" and wedges the entire
# provisioning chain before any stack script can run. Remove the stale
# entries here so apt-get update succeeds for repair on VMs carrying this
# legacy state. The same cleanup is also present inside stack-dotnet.sh
# to catch the rare case of `cloister add-stack dotnet` on a VM where
# base.sh has not been re-run since the legacy state appeared.
for stale in $(grep -lF '/usr/share/keyrings/microsoft-prod.gpg' /etc/apt/sources.list.d/*.list 2>/dev/null); do
  echo "  Removing stale Microsoft repo entry: $stale"
  sudo rm -f "$stale"
done

# A cloister-managed apt source whose publication has gone away -- a deleted
# Pages branch, a retired repository -- makes apt-get update exit non-zero,
# and under set -e that wedges the entire provisioning chain before a single
# package is installed. One optional component losing its repository must not
# cost the VM its base tools, so unreachable entries are disabled here. Each
# is re-added further down by the component that owns it, so a source that is
# merely having a bad minute comes straight back on this same run.
for list in /etc/apt/sources.list.d/*.list; do
  [ -f "$list" ] || continue
  url=$(awk '/^deb /{for (i = 1; i <= NF; i++) if ($i ~ /^https?:\/\//) {print $i; exit}}' "$list")
  suite=$(awk '/^deb /{for (i = 1; i <= NF; i++) if ($i ~ /^https?:\/\//) {print $(i + 1); exit}}' "$list")
  [ -n "$url" ] && [ -n "$suite" ] || continue
  if ! curl -fsSL --max-time 15 --retry 2 -o /dev/null "${url%/}/dists/${suite}/Release"; then
    echo "  Disabling unreachable apt source: $list ($url $suite)"
    sudo mv -f "$list" "$list.unreachable"
  fi
done

sudo apt-get update -q
sudo apt-get install -y -q git git-lfs curl wget jq direnv gpg build-essential

# Local gpg-agent policy. By default cloister masks the systemd-user units
# that auto-start a local gpg-agent at session login, so the runtime socket
# path /run/user/<uid>/gnupg/S.gpg-agent is free for the cloister-managed
# reverse tunnel that gpg_signing: true attaches. When the engine sets
# CLOISTER_GPG_LOCAL=1 (profile field gpg_local: true), we instead UNMASK
# the units so the user can run `gpg --gen-key`, import keys, decrypt files,
# etc. against a normal local agent. The two states cover three logical
# profile configurations:
#
#   gpg_signing: false, gpg_local: false  → mask (default; no signing path)
#   gpg_signing: true,  gpg_local: false  → mask (forwarded host agent)
#   gpg_signing: false, gpg_local: true   → unmask (in-VM key management)
#
# The gpg_signing + gpg_local combination is rejected at config load time.
#
# keyboxd.socket and dirmngr.socket are intentionally untouched: keyboxd
# manages the public-key database (required by gpg --import) and dirmngr
# handles keyserver lookups; both are independent of secret-key handling.
# systemctl is allowed to fail silently for the rare distro variant without
# a user systemd instance; on Ubuntu (cloister's default base image) it is
# reliably present.
GPG_AGENT_UNITS="gpg-agent.socket gpg-agent-extra.socket gpg-agent-ssh.socket gpg-agent-browser.socket gpg-agent.service"
if [ "${CLOISTER_GPG_LOCAL:-0}" = "1" ]; then
  echo "=== Enabling local gpg-agent (gpg_local: true) ==="
  # shellcheck disable=SC2086
  systemctl --user unmask $GPG_AGENT_UNITS 2>/dev/null || true
else
  echo "=== Disabling local gpg-agent (cloister forwards from the host or no signing) ==="
  # shellcheck disable=SC2086
  systemctl --user mask $GPG_AGENT_UNITS 2>/dev/null || true
fi

echo "=== Installing Node.js via NVM ==="
export NVM_DIR="$HOME/.nvm"
# Remove npmrc settings that conflict with nvm's prefix management.
if [ -f "$HOME/.npmrc" ]; then
  sed -i '/^prefix=/d; /^globalconfig=/d' "$HOME/.npmrc"
fi
set +euo pipefail
curl -fsSL -o /tmp/nvm-install.sh https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh
bash /tmp/nvm-install.sh
rm -f /tmp/nvm-install.sh
source "$NVM_DIR/nvm.sh"
nvm use --delete-prefix default --silent 2>/dev/null || true
nvm install --lts
set -euo pipefail

echo "=== Installing pnpm ==="
npm install -g pnpm

echo "=== Installing Claude Code ==="
# Skip when claude is already on PATH. Claude Code self-updates on every
# invocation, so re-running its installer during a repair has no functional
# effect; the installer's transient "install" subcommand peaks at roughly
# 3.7 GiB resident which OOM-kills on profiles configured with 4 GiB of
# memory and no swap, aborting the rest of the provisioning pipeline.
if command -v claude >/dev/null 2>&1; then
    echo "Claude Code already present at $(command -v claude); skipping installer."
else
    curl -fsSL -o /tmp/claude-install.sh https://claude.ai/install.sh
    bash /tmp/claude-install.sh
    rm -f /tmp/claude-install.sh
fi
export PATH="$HOME/.claude/bin:$PATH"

echo "=== Installing op-forward (1Password CLI forwarding) ==="
# op-forward is convenience tooling, not part of what makes the VM usable, so
# an unreachable package repository downgrades it to a warning rather than
# failing the provision. Its source entry is removed on the way out so the
# next apt-get update does not inherit the same failure.
if curl -fsSL --max-time 15 --retry 2 -o /tmp/op-forward.gpg https://ekovshilovsky.github.io/op-forward/key.gpg; then
  sudo rm -f /usr/share/keyrings/op-forward.gpg
  sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/op-forward.gpg /tmp/op-forward.gpg
  rm -f /tmp/op-forward.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/op-forward.gpg] https://ekovshilovsky.github.io/op-forward stable main" | sudo tee /etc/apt/sources.list.d/op-forward.list > /dev/null
  sudo apt-get update -q
  sudo apt-get install -y -q op-forward
  op-forward install --port 18340
else
  echo "  WARNING: the op-forward package repository is unreachable."
  echo "  1Password CLI forwarding will not be available in this VM."
  echo "  Re-run this provision once the repository is published again."
  sudo rm -f /etc/apt/sources.list.d/op-forward.list /etc/apt/sources.list.d/op-forward.list.unreachable
  rm -f /tmp/op-forward.gpg
fi

echo "=== Installing cloister-vm toolkit ==="
curl -fsSL -o /tmp/cloister.gpg https://ekovshilovsky.github.io/cloister/key.gpg
sudo rm -f /usr/share/keyrings/cloister.gpg
sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/cloister.gpg /tmp/cloister.gpg
rm -f /tmp/cloister.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/cloister.gpg] https://ekovshilovsky.github.io/cloister stable main" | sudo tee /etc/apt/sources.list.d/cloister.list > /dev/null
sudo apt-get update -q
sudo apt-get install -y -q cloister-vm

# In-VM cc-clip CLI. The clipboard tunnel forwards the host cc-clip daemon
# on :18339 into the VM, and cloister deploys the session token on entry,
# but the VM-side CLI client that actually invokes the daemon over the
# tunnel is a separate binary that must live in the VM. Installing it here
# on every base provision (including repair) makes the install idempotent
# and self-healing: a VM that lost the binary for any reason gets it back
# on the next repair.
#
# We download the prebuilt tarball from a pinned cc-clip GitHub release
# and verify its SHA256 against a hardcoded value, rather than executing
# the upstream install.sh from a mutable branch (`main`). The latter would
# be a supply-chain attack vector: a single push to ShunmeiCho/cc-clip
# main would propagate into every cloister VM on next repair, with
# passwordless sudo. The pinned tarball + SHA256 approach reduces the
# trust boundary to "this specific signed binary at this specific
# release," and bumping cc-clip requires an explicit cloister commit
# that updates both the version and the hashes — which goes through the
# normal review and release pipeline.
CC_CLIP_VERSION="0.7.7"
CC_CLIP_ARCH=$(dpkg --print-architecture)
case "$CC_CLIP_ARCH" in
    arm64) CC_CLIP_SHA256="0a37d3bb3274d62a0c5aec1d0036ad69b8d77fd9109584a4f98dc3861dd65abb" ;;
    amd64) CC_CLIP_SHA256="0dab6145bb9b2526b7f7cba511b669a690ce8000c25c1c34769c14fdd3dc800f" ;;
    *)
        echo "=== Skipping in-VM cc-clip CLI install (unsupported arch: $CC_CLIP_ARCH) ==="
        CC_CLIP_SHA256=""
        ;;
esac

if [ -n "$CC_CLIP_SHA256" ]; then
    if command -v cc-clip >/dev/null 2>&1 && cc-clip --version 2>/dev/null | grep -q "$CC_CLIP_VERSION"; then
        echo "=== cc-clip ${CC_CLIP_VERSION} already installed, skipping ==="
    else
        echo "=== Installing in-VM cc-clip CLI v${CC_CLIP_VERSION} (${CC_CLIP_ARCH}) ==="
        CC_CLIP_TARBALL="cc-clip_${CC_CLIP_VERSION}_linux_${CC_CLIP_ARCH}.tar.gz"
        CC_CLIP_URL="https://github.com/ShunmeiCho/cc-clip/releases/download/v${CC_CLIP_VERSION}/${CC_CLIP_TARBALL}"
        mkdir -p "$HOME/.local/bin"
        curl -fsSL -o "/tmp/${CC_CLIP_TARBALL}" "$CC_CLIP_URL"
        echo "${CC_CLIP_SHA256}  /tmp/${CC_CLIP_TARBALL}" | sha256sum -c -
        tar -xzf "/tmp/${CC_CLIP_TARBALL}" -C "$HOME/.local/bin/" cc-clip
        chmod +x "$HOME/.local/bin/cc-clip"
        rm -f "/tmp/${CC_CLIP_TARBALL}"
    fi
    # Deploy the xclip-name shim DIRECTLY rather than calling 'cc-clip
    # install'. The shim is a static shell script that intercepts the
    # specific xclip invocation Claude Code uses for image paste
    # (-selection clipboard -t image/png -o) and routes the read through
    # the host cc-clip daemon over the forwarded :18339 tunnel. cc-clip
    # ships a generic version of this script, but its 'install'
    # subcommand is non-idempotent ("install failed: shim already
    # installed; run 'cc-clip uninstall' first") and requires a brittle
    # uninstall+install dance on every repair, which churns filesystem
    # state and creates a window where the shim is absent. Writing the
    # shim verbatim from this script eliminates the dance entirely and
    # keeps the deploy idempotent: re-running 'cat > xclip ... chmod'
    # produces byte-identical output. The shim content is pinned to the
    # CC_CLIP_VERSION above and must be re-extracted from 'cc-clip
    # install' output when that version bumps.
    #
    # Text paste does not need a shim — the terminal emulator handles
    # text clipboard via OSC52 escape sequences at the terminal layer,
    # transparent to the VM. cc-clip's purpose is image clipboard only;
    # the shim's fallback-to-real-xclip path exists for non-image xclip
    # calls that nothing in a cloister VM actually makes.
    cat > "$HOME/.local/bin/xclip" <<'CC_CLIP_XCLIP_SHIM'
#!/bin/bash
# cc-clip xclip shim - intercepts Claude Code clipboard calls
# Installed by: cloister base.sh (pinned to cc-clip CC_CLIP_VERSION above)
# To regenerate: run 'cc-clip install' once on a clean VM and copy ~/.local/bin/xclip

set -euo pipefail

CC_CLIP_PORT="${CC_CLIP_PORT:-18339}"
CC_CLIP_ADDR="127.0.0.1:${CC_CLIP_PORT}"
CC_CLIP_TOKEN_FILE="${CC_CLIP_TOKEN_FILE:-${HOME}/.cache/cc-clip/session.token}"
CC_CLIP_SESSION_FILE="${CC_CLIP_SESSION_FILE:-${HOME}/.cache/cc-clip/session.id}"
CC_CLIP_PROBE_TIMEOUT_MS="${CC_CLIP_PROBE_TIMEOUT_MS:-500}"
CC_CLIP_FETCH_TIMEOUT_MS="${CC_CLIP_FETCH_TIMEOUT_MS:-5000}"
CC_CLIP_TOTAL_TIMEOUT_MS="${CC_CLIP_TOTAL_TIMEOUT_MS:-8000}"
REAL_XCLIP="/usr/bin/xclip"
_CC_CLIP_SELF_PATH="${BASH_SOURCE[0]:-$0}"
case "$_CC_CLIP_SELF_PATH" in
    */*) _CC_CLIP_SELF_DIR="${_CC_CLIP_SELF_PATH%/*}" ;;
    *) _CC_CLIP_SELF_DIR="$(pwd)" ;;
esac
if ! _CC_CLIP_SELF_DIR="$(cd "$_CC_CLIP_SELF_DIR" 2>/dev/null && pwd)"; then
    _CC_CLIP_SELF_DIR="$(pwd)"
fi
_CC_CLIP_SELF_FILE="$_CC_CLIP_SELF_DIR/${_CC_CLIP_SELF_PATH##*/}"

_cc_clip_log() {
    if [ "${CC_CLIP_DEBUG:-}" = "1" ]; then
        echo "cc-clip-shim: $*" >&2
    fi
}

_cc_clip_resolve_real_xclip() {
    if [ -n "${REAL_XCLIP:-}" ] && [ -x "$REAL_XCLIP" ]; then
        local real_parent real_name real_dir real_path
        case "$REAL_XCLIP" in
            */*) real_parent="${REAL_XCLIP%/*}"; real_name="${REAL_XCLIP##*/}" ;;
            *) real_parent="."; real_name="$REAL_XCLIP" ;;
        esac
        real_dir="$(cd "$real_parent" 2>/dev/null && pwd)" || real_dir=""
        real_path="$real_dir/$real_name"
        if [ "$real_path" != "$_CC_CLIP_SELF_FILE" ]; then
            printf '%s\n' "$REAL_XCLIP"
            return 0
        fi
    fi

    local old_ifs="$IFS"
    IFS=:
    local dir
    for dir in $PATH; do
        [ -n "$dir" ] || dir="."
        local abs_dir
        abs_dir="$(cd "$dir" 2>/dev/null && pwd)" || continue
        [ "$abs_dir" = "$_CC_CLIP_SELF_DIR" ] && continue
        if [ -x "$abs_dir/xclip" ] && [ ! -d "$abs_dir/xclip" ]; then
            IFS="$old_ifs"
            printf '%s\n' "$abs_dir/xclip"
            return 0
        fi
    done
    IFS="$old_ifs"
    return 1
}

_cc_clip_fallback() {
    local real_xclip
    if ! real_xclip="$(_cc_clip_resolve_real_xclip)"; then
        echo "cc-clip-shim: real xclip binary not found; install xclip or remove the cc-clip shim" >&2
        exit 127
    fi
    _cc_clip_log "falling back to real xclip: $real_xclip $*"
    exec "$real_xclip" "$@"
}

_cc_clip_read_token() {
    if [ ! -f "$CC_CLIP_TOKEN_FILE" ]; then
        return 1
    fi
    cat "$CC_CLIP_TOKEN_FILE"
}

_cc_clip_session_header() {
    if [ -f "$CC_CLIP_SESSION_FILE" ]; then
        echo "X-CC-Clip-Session: $(cat "$CC_CLIP_SESSION_FILE" 2>/dev/null)"
    fi
}

_cc_clip_curl_config() {
    local token="$1"
    local session_hdr="${2:-}"
    printf 'header = "Authorization: Bearer %s"\n' "$token"
    printf 'header = "User-Agent: cc-clip/0.1"\n'
    if [ -n "$session_hdr" ]; then
        printf 'header = "%s"\n' "$session_hdr"
    fi
}

_cc_clip_probe() {
    local timeout_s
    timeout_s=$(awk "BEGIN {printf \"%f\", ${CC_CLIP_PROBE_TIMEOUT_MS}/1000}")
    if command -v timeout >/dev/null 2>&1; then
        timeout "$timeout_s" bash -c "echo >/dev/tcp/${CC_CLIP_ADDR%%:*}/${CC_CLIP_ADDR##*:}" 2>/dev/null
    elif command -v nc >/dev/null 2>&1; then
        nc -z -w 1 "${CC_CLIP_ADDR%%:*}" "${CC_CLIP_ADDR##*:}" 2>/dev/null
    else
        bash -c "echo >/dev/tcp/${CC_CLIP_ADDR%%:*}/${CC_CLIP_ADDR##*:}" 2>/dev/null
    fi
}

# Fetch JSON endpoint (text-safe, small payloads only)
_cc_clip_fetch_json() {
    local path="$1"
    local token
    token=$(_cc_clip_read_token) || return 12
    local timeout_s
    timeout_s=$(awk "BEGIN {printf \"%f\", ${CC_CLIP_FETCH_TIMEOUT_MS}/1000}")
    local session_hdr
    session_hdr=$(_cc_clip_session_header)
    _cc_clip_curl_config "$token" "$session_hdr" | curl -sf --max-time "$timeout_s" \
        -K - \
        "http://${CC_CLIP_ADDR}${path}"
}

# Fetch binary to temp file, then cat to stdout (preserves NUL bytes, allows fallback)
_cc_clip_fetch_binary() {
    local path="$1"
    local token
    token=$(_cc_clip_read_token) || return 12
    local timeout_s
    timeout_s=$(awk "BEGIN {printf \"%f\", ${CC_CLIP_FETCH_TIMEOUT_MS}/1000}")
    local session_hdr
    session_hdr=$(_cc_clip_session_header)
    local tmpfile
    tmpfile=$(mktemp 2>/dev/null) || return 20
    if _cc_clip_curl_config "$token" "$session_hdr" | curl -sf --max-time "$timeout_s" \
        -o "$tmpfile" \
        -K - \
        "http://${CC_CLIP_ADDR}${path}"; then
        # Guard against empty response (e.g. HTTP 204 No Content)
        if [ ! -s "$tmpfile" ]; then
            _cc_clip_log "fetch returned empty body"
            rm -f "$tmpfile"
            return 10
        fi
        cat "$tmpfile"
        rm -f "$tmpfile"
        return 0
    else
        local rc=$?
        rm -f "$tmpfile"
        return $rc
    fi
}

# Parse arguments to detect Claude Code invocation patterns
ARGS="$*"

case "$ARGS" in
    *"-selection clipboard"*"-t TARGETS"*"-o"*)
        # Claude checks clipboard targets
        _cc_clip_log "intercepting TARGETS check"
        if _cc_clip_probe; then
            RESULT=$(_cc_clip_fetch_json "/clipboard/type" 2>/dev/null) || {
                _cc_clip_log "fetch type failed, exit=$?"
                _cc_clip_fallback "$@"
            }
            TYPE=$(echo "$RESULT" | grep -o '"type":"[^"]*"' | head -1 | cut -d'"' -f4)
            if [ "$TYPE" = "image" ]; then
                FORMAT=$(echo "$RESULT" | grep -o '"format":"[^"]*"' | head -1 | cut -d'"' -f4)
                echo "image/${FORMAT:-png}"
                exit 0
            fi
            _cc_clip_fallback "$@"
        else
            _cc_clip_log "tunnel not reachable"
            _cc_clip_fallback "$@"
        fi
        ;;

    *"-selection clipboard"*"-t image/"*"-o"*)
        # Claude reads clipboard image — fetch to temp file then cat (binary-safe + fallback-safe)
        _cc_clip_log "intercepting image read"
        if _cc_clip_probe; then
            if _cc_clip_fetch_binary "/clipboard/image"; then
                exit 0
            fi
            _cc_clip_log "fetch image failed, falling back"
            _cc_clip_fallback "$@"
        else
            _cc_clip_log "tunnel not reachable"
            _cc_clip_fallback "$@"
        fi
        ;;

    *)
        # All other invocations: pass through
        _cc_clip_fallback "$@"
        ;;
esac
CC_CLIP_XCLIP_SHIM
    chmod 0755 "$HOME/.local/bin/xclip"
fi

echo "=== Base provisioning complete ==="
