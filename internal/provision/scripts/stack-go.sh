#!/bin/bash
set -euo pipefail
echo "=== Installing Go stack ==="
GO_VERSION=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)
curl -fsSL "https://go.dev/dl/${GO_VERSION}.linux-arm64.tar.gz" | sudo tar -C /usr/local -xzf -
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.bashrc
echo "=== Go stack complete ==="
