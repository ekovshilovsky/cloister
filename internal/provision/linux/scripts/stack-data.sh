#!/bin/bash
set -euo pipefail

# Source NVM so npm/node are available (installed by base provisioning)
export NVM_DIR="$HOME/.nvm"
[ -s "$NVM_DIR/nvm.sh" ] && source "$NVM_DIR/nvm.sh"

echo "=== Installing data stack ==="
sudo apt-get install -y -qq postgresql-client jq
npm install -g mongosh
echo "=== Data stack complete ==="
