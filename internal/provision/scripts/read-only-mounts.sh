#!/bin/bash
for dir in .ssh .gnupg Downloads; do
    mount_point="$HOME/$dir"
    if mountpoint -q "$mount_point" 2>/dev/null; then
        sudo mount -o remount,ro "$mount_point" 2>/dev/null || true
    fi
done
