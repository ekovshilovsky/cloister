#!/bin/bash
set -euo pipefail
echo "=== Installing Rust stack ==="
curl --proto '=https' --tlsv1.2 -sSf -o /tmp/rustup-init.sh https://sh.rustup.rs
sh /tmp/rustup-init.sh -y
rm -f /tmp/rustup-init.sh
echo "=== Rust stack complete ==="
