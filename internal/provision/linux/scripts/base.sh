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
curl -fsSL -o /tmp/op-forward.gpg https://ekovshilovsky.github.io/op-forward/key.gpg
sudo rm -f /usr/share/keyrings/op-forward.gpg
sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/op-forward.gpg /tmp/op-forward.gpg
rm -f /tmp/op-forward.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/op-forward.gpg] https://ekovshilovsky.github.io/op-forward stable main" | sudo tee /etc/apt/sources.list.d/op-forward.list > /dev/null
sudo apt-get update -q
sudo apt-get install -y -q op-forward
op-forward install --port 18340

echo "=== Installing cloister-vm toolkit ==="
curl -fsSL -o /tmp/cloister.gpg https://ekovshilovsky.github.io/cloister/key.gpg
sudo rm -f /usr/share/keyrings/cloister.gpg
sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/cloister.gpg /tmp/cloister.gpg
rm -f /tmp/cloister.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/cloister.gpg] https://ekovshilovsky.github.io/cloister stable main" | sudo tee /etc/apt/sources.list.d/cloister.list > /dev/null
sudo apt-get update -q
sudo apt-get install -y -q cloister-vm

echo "=== Base provisioning complete ==="
