#!/bin/bash
set -euo pipefail
DOTNET_VERSION="${DOTNET_VERSION:-10.0}"
echo "=== Installing .NET ${DOTNET_VERSION} stack ==="
curl -fsSL -o /tmp/dotnet-install.sh https://dot.net/v1/dotnet-install.sh
sudo bash /tmp/dotnet-install.sh --channel "${DOTNET_VERSION}" --install-dir /usr/share/dotnet
rm -f /tmp/dotnet-install.sh
sudo ln -sf /usr/share/dotnet/dotnet /usr/local/bin/dotnet
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
echo "  dotnet               - .NET SDK"
echo "  psql                 - PostgreSQL client"
echo "  sqlcmd, bcp          - SQL Server client (at /opt/mssql-tools18/bin)"
