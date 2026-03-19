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

echo "=== Base provisioning complete ==="
