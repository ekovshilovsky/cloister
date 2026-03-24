#!/bin/bash
# Pull the Docker image for the configured agent runtime and install
# a cron job to clean up stale temp files in the agent data directory.
set -euo pipefail

IMAGE="${AGENT_IMAGE:?AGENT_IMAGE must be set}"

echo "=== Pulling agent Docker image ==="
docker pull "$IMAGE"

echo "=== Installing agent tmp cleanup cron ==="
if ! command -v crontab &>/dev/null; then
    sudo apt-get install -y -qq cron 2>/dev/null || true
fi
if command -v crontab &>/dev/null; then
    CRON_CMD="0 3 * * * find /home/*/\.openclaw/tmp -type f -mtime +7 -delete 2>/dev/null"
    (crontab -l 2>/dev/null | grep -v "openclaw/tmp" ; echo "$CRON_CMD") | crontab -
else
    echo "Warning: cron not available, skipping tmp cleanup scheduling"
fi

echo "=== Agent setup complete ==="
