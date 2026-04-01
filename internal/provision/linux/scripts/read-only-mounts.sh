#!/bin/bash
# Enforce read-only access on sensitive mounted directories.
READONLY_DIRS=".ssh .gnupg Downloads .ollama/models"

# For headless profiles, also enforce read-only on Claude extension directories
# to prevent lateral movement attacks where a compromised agent writes malicious
# plugins that are then loaded by interactive profiles.
if [ "${CLOISTER_HEADLESS:-}" = "1" ]; then
    READONLY_DIRS="$READONLY_DIRS .claude/plugins/cache .claude/plugins/marketplaces .claude/skills .claude/agents .agents"
fi

for dir in $READONLY_DIRS; do
    mount_point="$HOME/$dir"
    if mountpoint -q "$mount_point" 2>/dev/null; then
        sudo mount -o remount,ro "$mount_point" 2>/dev/null || true
    fi
done
