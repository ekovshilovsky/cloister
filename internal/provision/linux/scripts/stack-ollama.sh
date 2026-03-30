#!/bin/bash
set -euo pipefail
echo "=== Installing Ollama stack ==="

# Install zstd — required by the Ollama installer for archive extraction
sudo apt-get update -qq
sudo apt-get install -y -qq zstd

# Install Ollama CLI and server binary via the official installer
curl -fsSL -o /tmp/ollama-install.sh https://ollama.com/install.sh
sh /tmp/ollama-install.sh
rm -f /tmp/ollama-install.sh

# Disable the local Ollama server — inference runs on the host via SSH tunnel.
# The host's Metal GPU provides hardware-accelerated inference; running a
# second server inside the VM on CPU would be redundant and slow.
sudo systemctl stop ollama 2>/dev/null || true
sudo systemctl disable ollama 2>/dev/null || true

echo "=== Ollama stack complete ==="
