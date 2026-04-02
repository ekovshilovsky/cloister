#!/bin/bash
set -euo pipefail
echo "=== Installing base tools ==="
sudo apt-get update -q
sudo apt-get install -y -q git git-lfs curl wget jq direnv gpg pinentry-curses build-essential

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
curl -fsSL -o /tmp/claude-install.sh https://claude.ai/install.sh
bash /tmp/claude-install.sh
rm -f /tmp/claude-install.sh
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
