#!/bin/bash
set -euo pipefail
echo "=== Installing base tools ==="
sudo apt-get update -qq
sudo apt-get install -y -qq git git-lfs curl wget jq direnv gpg pinentry-curses build-essential

echo "=== Installing Node.js via NVM ==="
export NVM_DIR="$HOME/.nvm"
set +euo pipefail
curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh | bash
source "$NVM_DIR/nvm.sh"
nvm install --lts
set -euo pipefail

echo "=== Installing pnpm ==="
npm install -g pnpm

echo "=== Installing Claude Code ==="
curl -fsSL https://claude.ai/install.sh | bash
export PATH="$HOME/.claude/bin:$PATH"

echo "=== Installing op-forward (1Password CLI forwarding) ==="
curl -fsSL https://ekovshilovsky.github.io/op-forward/key.gpg | sudo gpg --dearmor -o /usr/share/keyrings/op-forward.gpg 2>/dev/null
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/op-forward.gpg] https://ekovshilovsky.github.io/op-forward stable main" | sudo tee /etc/apt/sources.list.d/op-forward.list > /dev/null
sudo apt-get update -qq
sudo apt-get install -y -qq op-forward
op-forward install --port 18340

echo "=== Installing cloister-vm toolkit ==="
curl -fsSL https://ekovshilovsky.github.io/cloister/key.gpg | sudo gpg --dearmor -o /usr/share/keyrings/cloister.gpg 2>/dev/null
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/cloister.gpg] https://ekovshilovsky.github.io/cloister stable main" | sudo tee /etc/apt/sources.list.d/cloister.list > /dev/null
sudo apt-get update -qq
sudo apt-get install -y -qq cloister-vm

echo "=== Base provisioning complete ==="
