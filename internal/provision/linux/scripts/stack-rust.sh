#!/bin/bash
set -euo pipefail
echo "=== Installing Rust stack ==="
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
echo "=== Rust stack complete ==="
