#!/bin/bash
set -euo pipefail
DOTNET_VERSION="${DOTNET_VERSION:-10.0}"
echo "=== Installing .NET ${DOTNET_VERSION} stack ==="
curl -fsSL -o /tmp/dotnet-install.sh https://dot.net/v1/dotnet-install.sh
sudo bash /tmp/dotnet-install.sh --channel "${DOTNET_VERSION}" --install-dir /usr/share/dotnet
rm -f /tmp/dotnet-install.sh
sudo ln -sf /usr/share/dotnet/dotnet /usr/local/bin/dotnet
sudo apt-get install -y -q postgresql-client
echo "=== .NET stack complete ==="
