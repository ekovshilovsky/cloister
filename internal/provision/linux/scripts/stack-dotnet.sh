#!/bin/bash
set -euo pipefail
DOTNET_VERSION="${DOTNET_VERSION:-10.0}"
echo "=== Installing .NET ${DOTNET_VERSION} stack ==="

# dotnet-install.sh is a per-user installer by design. Running it under
# sudo with --install-dir /usr/share/dotnet (the previous cloister
# approach) has a known footgun: the launcher binary at
# <install-dir>/dotnet can land at 0 bytes — even when the SDK
# directories underneath (host/, packs/, sdk/, shared/, templates/) are
# fully populated — if the sudo boundary interrupts the launcher write
# step. The user is then left with a complete SDK that bash refuses to
# execute because /usr/share/dotnet/dotnet is empty.
#
# Install per-user instead. PATH wiring lives in the cloister-managed
# bashrc (see templates/bashrc.tmpl), which conditionally appends
# $HOME/.dotnet to PATH when the directory exists.
curl -fsSL -o /tmp/dotnet-install.sh https://dot.net/v1/dotnet-install.sh
bash /tmp/dotnet-install.sh --channel "${DOTNET_VERSION}" --install-dir "$HOME/.dotnet"
rm -f /tmp/dotnet-install.sh

# Validate the launcher is non-empty so a truncated install fails the
# stack script loudly here, rather than silently leaving the rest of
# provisioning to discover the broken binary later.
if [ ! -s "$HOME/.dotnet/dotnet" ]; then
    echo "dotnet install left a 0-byte launcher at $HOME/.dotnet/dotnet" >&2
    echo "this usually means the dotnet-install.sh download was interrupted; re-run the stack" >&2
    exit 1
fi

sudo apt-get install -y -q postgresql-client

# Microsoft SQL Server client tooling (sqlcmd, bcp, ODBC driver). The .NET
# ecosystem pairs natively with SQL Server, so installing these alongside
# the .NET SDK is the lowest-friction way to ensure dotnet profiles can
# query their backing databases without extra setup. The Microsoft apt
# repo is per-distro-version; we look up the current release dynamically
# so the same script works across Ubuntu 22.04, 24.04, etc.
. /etc/os-release
MS_PROD_VERSION="${VERSION_ID}"
MS_PROD_CODENAME="${VERSION_CODENAME}"
echo "=== Installing mssql-tools18 + msodbcsql18 (Microsoft prod repo for ${MS_PROD_VERSION}/${MS_PROD_CODENAME}) ==="
sudo install -d /etc/apt/keyrings
curl -fsSL https://packages.microsoft.com/keys/microsoft.asc | sudo gpg --batch --yes --dearmor -o /etc/apt/keyrings/microsoft.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/microsoft.gpg] https://packages.microsoft.com/ubuntu/${MS_PROD_VERSION}/prod ${MS_PROD_CODENAME} main" | sudo tee /etc/apt/sources.list.d/microsoft-prod.list > /dev/null
sudo apt-get update -q
sudo ACCEPT_EULA=Y apt-get install -y -q mssql-tools18 msodbcsql18 unixodbc-dev

echo "=== .NET stack complete ==="
echo
echo "Tools available (PATH wired via the cloister-managed bashrc):"
echo "  dotnet               - .NET SDK (per-user install at \$HOME/.dotnet)"
echo "  psql                 - PostgreSQL client"
echo "  sqlcmd, bcp          - SQL Server client (at /opt/mssql-tools18/bin)"
