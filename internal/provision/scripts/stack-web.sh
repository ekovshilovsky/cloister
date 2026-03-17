#!/bin/bash
set -euo pipefail
echo "=== Installing web stack ==="
# Playwright + Chromium
sudo apt-get install -y -qq chromium-browser
sudo mkdir -p /opt/google/chrome
sudo ln -sf /usr/bin/chromium-browser /opt/google/chrome/chrome
# GitHub CLI
(type gh >/dev/null 2>&1) || {
  curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null
  sudo apt-get update -qq && sudo apt-get install -y -qq gh
}
# Vercel CLI
npm install -g vercel
echo "=== Web stack complete ==="
