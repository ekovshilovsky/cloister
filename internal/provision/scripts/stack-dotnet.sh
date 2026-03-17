#!/bin/bash
set -euo pipefail
DOTNET_VERSION="${DOTNET_VERSION:-10.0}"
echo "=== Installing .NET ${DOTNET_VERSION} stack ==="
wget -qO- https://dot.net/v1/dotnet-install.sh | bash -s -- --channel "${DOTNET_VERSION}" --install-dir /usr/share/dotnet
sudo ln -sf /usr/share/dotnet/dotnet /usr/local/bin/dotnet
sudo apt-get install -y -qq postgresql-client
echo "=== .NET stack complete ==="
