#!/bin/bash
set -euo pipefail
echo "=== Installing data stack ==="
sudo apt-get install -y -qq postgresql-client jq
npm install -g mongosh
echo "=== Data stack complete ==="
