#!/bin/bash
set -euo pipefail
echo "=== Installing Go stack ==="
GO_VERSION=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)
curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/${GO_VERSION}.linux-arm64.tar.gz"
sudo tar -C /usr/local -xzf /tmp/go.tar.gz
rm -f /tmp/go.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.bashrc
echo "=== Go stack complete ==="
