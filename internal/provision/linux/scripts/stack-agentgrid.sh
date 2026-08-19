#!/bin/bash
set -euo pipefail

echo "=== Installing Agent Grid stack (headless daemon) ==="

# Map uname -m to the Debian architecture tag used in Agent Grid .deb names.
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) DEB_ARCH="amd64" ;;
  aarch64|arm64) DEB_ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH" >&2
    echo "Agent Grid Linux packages are published as amd64 or arm64 .debs." >&2
    exit 1
    ;;
esac

# Locate a locally provided package before reaching for the network. Cloister
# mounts the host's Downloads folder read-only into the VM, so a .deb dropped
# there (including pre-release builds) installs without any download. An
# explicit AGENT_GRID_DEB path wins over the Downloads scan.
find_local_deb() {
  if [[ -n "${AGENT_GRID_DEB:-}" ]]; then
    if [[ -f "$AGENT_GRID_DEB" ]]; then
      printf '%s\n' "$AGENT_GRID_DEB"
      return 0
    fi
    echo "AGENT_GRID_DEB is set but not a file: $AGENT_GRID_DEB" >&2
    return 1
  fi
  local dir candidate
  for dir in "$HOME/Downloads" /Users/*/Downloads; do
    [[ -d "$dir" ]] || continue
    candidate="$(ls -t "$dir"/AgentGrid-*-"$DEB_ARCH".deb 2>/dev/null | head -1)"
    if [[ -n "$candidate" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

if DEB="$(find_local_deb)"; then
  echo "Installing Agent Grid from local package: $DEB"
else
  # Resolve the release exactly as Agent Grid publishes it. An explicit
  # AGENT_GRID_VERSION is useful for reproducible VM builds; otherwise install
  # the latest GitHub release. The arm64 filename follows the same convention as
  # the existing amd64 asset and will begin working as soon as upstream publishes
  # that artifact.
  RELEASE_REPO="agent-grid/agent-grid-releases"
  if [[ -n "${AGENT_GRID_VERSION:-}" ]]; then
    VERSION="${AGENT_GRID_VERSION#v}"
    TAG="v${VERSION}"
  else
    RELEASE_API="https://api.github.com/repos/agent-grid/agent-grid-releases/releases/latest"
    RELEASE_JSON="$(curl -fsSL \
      -H "Accept: application/vnd.github+json" \
      -H "X-GitHub-Api-Version: 2022-11-28" \
      "$RELEASE_API")"
    TAG="$(printf '%s' "$RELEASE_JSON" |
      sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' |
      head -1)"
    if [[ -z "$TAG" ]]; then
      echo "Could not resolve the latest Agent Grid release tag from $RELEASE_API" >&2
      exit 1
    fi
    VERSION="${TAG#v}"
  fi

  ASSET_NAME="AgentGrid-${VERSION}-${DEB_ARCH}.deb"
  DOWNLOAD_URL="https://github.com/${RELEASE_REPO}/releases/download/${TAG}/${ASSET_NAME}"
  DEB="$WORKDIR/$ASSET_NAME"

  echo "Downloading Agent Grid ${VERSION} (${DEB_ARCH})..."
  if ! curl -fL --retry 3 --retry-delay 2 -o "$DEB" "$DOWNLOAD_URL"; then
    echo "Agent Grid release asset not found: $DOWNLOAD_URL" >&2
    echo "Alternatively place an AgentGrid-<version>-${DEB_ARCH}.deb in ~/Downloads" >&2
    echo "on the host, or point AGENT_GRID_DEB at a package file, and re-run." >&2
    exit 1
  fi
fi

# Runtime libraries declared by the electron-builder .deb, plus xvfb for any
# accidental GUI spawn paths. The headless daemon itself uses ELECTRON_RUN_AS_NODE
# and does not need a display.
sudo apt-get update -q
sudo apt-get install -y -q \
  libgtk-3-0 \
  libnotify4 \
  libnss3 \
  libxss1 \
  libxtst6 \
  xdg-utils \
  libatspi2.0-0 \
  libuuid1 \
  libsecret-1-0 \
  xvfb

# Ubuntu 24.04 renamed the ALSA package; fall back for older guests.
if ! sudo apt-get install -y -q libasound2t64 2>/dev/null; then
  sudo apt-get install -y -q libasound2 || true
fi

# Force-overwrite is fine: this stack owns /opt/Agent Grid.
#
# Remove any unpacked app directory first: Electron prefers resources/app/
# over resources/app.asar, so a leftover directory from a previous manual or
# development install would silently shadow the package being installed.
sudo rm -rf "/opt/Agent Grid/resources/app"
sudo dpkg -i "$DEB" || sudo apt-get install -f -y -q

AG_ROOT="/opt/Agent Grid"
AG_BIN="$AG_ROOT/Agent Grid"
DAEMON_MAIN="$AG_ROOT/resources/app.asar/electron-dist/daemon/standalone/main.js"

if [[ ! -x "$AG_BIN" ]]; then
  echo "Expected Electron binary missing: $AG_BIN" >&2
  exit 1
fi
# The daemon entry lives inside app.asar, so a plain bash -f test cannot see
# it (stat on an asar-internal path fails with ENOTDIR). Ask Electron itself:
# under ELECTRON_RUN_AS_NODE its patched fs resolves asar-internal paths, the
# same way the daemon wrapper loads the entry at runtime.
if ! ELECTRON_RUN_AS_NODE=1 "$AG_BIN" -e 'require("fs").accessSync(process.argv[1])' "$DAEMON_MAIN" 2>/dev/null; then
  # The published 2.8.x layout uses the path above. Fail loudly if absent so
  # we do not install a broken wrapper.
  echo "Daemon entry missing: $DAEMON_MAIN" >&2
  echo "This Agent Grid build may not ship the headless daemon. Upgrade the .deb." >&2
  exit 1
fi

# PATH wrappers. ELECTRON_RUN_AS_NODE is required so native addons load against
# Electron's ABI rather than a system Node.
#
# Set both data-dir env vars to the same non-dev path. Some Agent Grid builds
# treat a "-dev" suffix specially for port selection; cloister keeps a stable
# production-style data dir under ~/.agent-grid-daemon.
sudo tee /usr/local/bin/agent-grid-daemon >/dev/null <<'WRAPPER'
#!/bin/bash
set -euo pipefail
AG_ROOT="/opt/Agent Grid"
export ELECTRON_RUN_AS_NODE=1
export AGENT_GRID_DAEMON_DATA_DIR="${AGENT_GRID_DAEMON_DATA_DIR:-$HOME/.agent-grid-daemon}"
export AGENT_GRID_USER_DATA_DIR="${AGENT_GRID_USER_DATA_DIR:-$AGENT_GRID_DAEMON_DATA_DIR}"

# The Claude SDK worker backend runs a native binary bundled inside
# app.asar.unpacked, not a PATH CLI. Under ELECTRON_RUN_AS_NODE the daemon
# cannot self-resolve it (app.isPackaged is false), so it falls back inside
# app.asar and spawning dies with ENOTDIR. Point it at the arch/libc-correct
# binary here; bash handles the space in the path that systemd Environment=
# would split on. Resolved at launch so it survives package upgrades.
if [[ -z "${AGENT_GRID_CLAUDE_CLI_PATH:-}" ]]; then
  case "$(uname -m)" in
    aarch64 | arm64) sdk_arch="arm64" ;;
    x86_64 | amd64) sdk_arch="x64" ;;
    *) sdk_arch="" ;;
  esac
  if [[ -n "$sdk_arch" ]]; then
    sdk_libc=""
    if ls /lib/ld-musl-* >/dev/null 2>&1; then sdk_libc="-musl"; fi
    sdk_cli="$AG_ROOT/resources/app.asar.unpacked/node_modules/@anthropic-ai/claude-agent-sdk-linux-${sdk_arch}${sdk_libc}/claude"
    if [[ -x "$sdk_cli" ]]; then export AGENT_GRID_CLAUDE_CLI_PATH="$sdk_cli"; fi
  fi
fi

exec "$AG_ROOT/Agent Grid" \
  "$AG_ROOT/resources/app.asar/electron-dist/daemon/standalone/main.js" \
  "$@"
WRAPPER
sudo chmod 755 /usr/local/bin/agent-grid-daemon

sudo tee /usr/local/bin/agent-grid-pair >/dev/null <<'WRAPPER'
#!/bin/bash
set -euo pipefail
exec /usr/local/bin/agent-grid-daemon pair "$@"
WRAPPER
sudo chmod 755 /usr/local/bin/agent-grid-pair

# systemd --user unit so the daemon survives logout and restarts on crash.
# Idle auto-exit behavior is product-defined; Restart=always brings it back.
# Prefer keeping a client attached for long runs.
#
# Guest listen port stays at the daemon default (8765). cloister remaps a free
# Mac-side port via ssh -L on profile entry so it does not collide with the
# Mac desktop app.
mkdir -p "$HOME/.config/systemd/user"
UNIT_FILE="$HOME/.config/systemd/user/agent-grid-daemon.service"
OLD_UNIT_HASH=""
if [[ -f "$UNIT_FILE" ]]; then
  OLD_UNIT_HASH="$(sha256sum "$UNIT_FILE" | cut -d' ' -f1)"
fi
cat > "$UNIT_FILE" <<'UNIT'
[Unit]
Description=Agent Grid headless daemon
After=network.target

[Service]
Type=simple
Environment=ELECTRON_RUN_AS_NODE=1
# Marks this process as the daemon. The desktop app stamps this when IT
# spawns the daemon; under systemd there is no app to do that, so the unit
# must set it. Without it daemonClientEnabled() is false in every master
# agent the daemon spawns, and worker spawning is silently blocked.
Environment=AGENT_GRID_DAEMON=1
Environment=AGENT_GRID_DAEMON_DATA_DIR=%h/.agent-grid-daemon
Environment=AGENT_GRID_USER_DATA_DIR=%h/.agent-grid-daemon
# Opt-in public daemon flag so Mac clients can reach the VM over Cloister's
# SSH local forward (host :18765 → VM :8765). Harmless if the installed build
# ignores the variable until upstream supports it.
Environment=AGENT_GRID_ALLOW_LOCAL_FORWARD=1
# Keep the headless daemon up for unattended VM use. 0 disables the idle
# self-exit so a paired Mac/phone client can reconnect at any time without a
# window where the daemon has quit. Builds without this knob ignore it and fall
# back to their built-in idle behavior; Restart=always still recovers.
Environment=AGENT_GRID_IDLE_SHUTDOWN_MS=0
# Agent CLIs (claude, cursor-agent, ...) install to ~/.local/bin, which the
# systemd default PATH does not include. Without it the daemon's `which`
# probes report every agent as "not found" even when installed.
Environment=PATH=%h/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ExecStart=/usr/local/bin/agent-grid-daemon run --allow-local-forward --idle-shutdown-ms 0
Restart=always
RestartSec=3
KillMode=control-group
TimeoutStopSec=20

[Install]
WantedBy=default.target
UNIT

# Allow user services without an interactive login (headless VM use).
if command -v loginctl >/dev/null 2>&1; then
  sudo loginctl enable-linger "$(id -un)" || true
fi

systemctl --user daemon-reload
systemctl --user enable agent-grid-daemon.service
# Enabling with immediate start is a no-op for an already-running service,
# so a re-run of this script (cloister repair / addstack) would leave a live
# daemon on the old unit settings (PATH, env knobs) after the file changed.
# Restart when the unit content actually changed; otherwise just make sure
# the daemon is up without disturbing connected clients.
NEW_UNIT_HASH="$(sha256sum "$UNIT_FILE" | cut -d' ' -f1)"
if [[ "$OLD_UNIT_HASH" != "$NEW_UNIT_HASH" ]]; then
  systemctl --user restart agent-grid-daemon.service
else
  systemctl --user start agent-grid-daemon.service
fi

# Readiness wait so pair/guidance can assume the port is up. A first-run
# Electron cold start in a fresh VM can take well over five seconds, so give
# it a generous window before declaring failure.
for _ in $(seq 1 120); do
  if ss -ltn 2>/dev/null | grep -q ':8765 '; then
    break
  fi
  sleep 0.25
done

if ! systemctl --user is-active --quiet agent-grid-daemon.service ||
   ! ss -ltn 2>/dev/null | grep -q ':8765 '; then
  echo "Agent Grid daemon failed to start." >&2
  systemctl --user status agent-grid-daemon.service --no-pager >&2 || true
  journalctl --user -u agent-grid-daemon.service -n 50 --no-pager >&2 || true
  exit 1
fi

# Agent CLIs. Adding the agentgrid stack means the user wants a full remote
# agent host, so install every harness Agent Grid can drive; anyone who does
# not add the stack is unaffected. Each CLI lands in ~/.local/bin, already on
# the daemon's PATH, so no restart is needed (the daemon `which`-probes live).
# Idempotent (skip if present) to keep repair cheap; non-fatal per agent so a
# vendor being down cannot fail provisioning; non-interactive and time-bounded
# so a script that expects a prompt cannot wedge the run. Logging in and API
# keys are done afterward from the Agent Grid client against the daemon.
#
# These are third-party install scripts fetched at provision time; that
# supply-chain surface is the deliberate cost of a one-command agent host.
export PATH="$HOME/.local/bin:$PATH"

install_agent_cli() {
  local name="$1" binary="$2" cmd="$3"

  if command -v "$binary" >/dev/null 2>&1; then
    echo "  $name: present"
    return 0
  fi

  echo "  $name: installing..."
  if timeout 300 bash -c "$cmd" </dev/null >/dev/null 2>&1 && command -v "$binary" >/dev/null 2>&1; then
    echo "  $name: installed"
  else
    echo "  $name: install failed or timed out — add it later from the client" >&2
  fi

  return 0
}

echo "Installing agent CLIs..."
install_agent_cli "Claude Code" claude       'curl -fsSL https://claude.ai/install.sh | bash'
install_agent_cli "Codex"       codex        'curl -fsSL https://chatgpt.com/codex/install.sh | sh'
install_agent_cli "Cursor"      cursor-agent 'curl https://cursor.com/install -fsS | bash'
install_agent_cli "OpenCode"    opencode     'curl -fsSL https://opencode.ai/install | bash'
install_agent_cli "Antigravity" agy          'curl -fsSL https://antigravity.google/cli/install.sh | sh'
install_agent_cli "Kimi"        kimi         'curl -fsSL https://code.kimi.com/kimi-code/install.sh | bash'
install_agent_cli "Grok"        grok         'curl -fsSL https://x.ai/cli/install.sh | bash'
install_agent_cli "Devin"       devin        'curl -fsSL https://cli.devin.ai/install.sh | bash'
install_agent_cli "Pi"          pi           'curl -fsSL https://pi.dev/install.sh | sh'

echo "=== Agent Grid stack complete ==="
echo
echo "Daemon:"
echo "  systemctl --user status agent-grid-daemon"
echo "  agent-grid-daemon run          # foreground, for debugging"
echo "  agent-grid-pair --label cloister-<profile>   # mint a 6-char pairing code"
echo
echo "Data dir: ~/.agent-grid-daemon"
echo "Listen:   0.0.0.0:8765 (inside the VM)"
echo
echo "cloister remaps this to a free Mac-side port (default 127.0.0.1:18765)"
echo "on profile entry, because the Mac desktop app keeps its own daemon on"
echo "8765 and Colima auto-forwards guest ports to the host on the same number."
echo "In the Agent Grid client: Settings → Devices → Connect to a device,"
echo "host 127.0.0.1:18765 plus the pairing code."
echo
echo "Idle policy: cloister runs the daemon with --idle-shutdown-ms 0"
echo "(AGENT_GRID_IDLE_SHUTDOWN_MS=0), so it stays up until the VM or the"
echo "systemd user service stops. Builds without that knob fall back to their"
echo "built-in idle exit; systemd Restart=always brings the daemon back."
