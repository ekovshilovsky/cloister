#!/bin/bash
set -euo pipefail
echo "=== Setting up GPG isolation ==="
GPG_LOCAL="$HOME/.gnupg-local"
GPG_HOST="$HOME/.gnupg"

if [ ! -d "$GPG_LOCAL" ]; then
    mkdir -p "$GPG_LOCAL"
    chmod 700 "$GPG_LOCAL"
    # Copy key material from host mount
    cp -r "$GPG_HOST/private-keys-v1.d" "$GPG_LOCAL/" 2>/dev/null || true
    cp "$GPG_HOST/trustdb.gpg" "$GPG_LOCAL/" 2>/dev/null || true
    # Disable keyboxd (prevents lock contention with host)
    echo "no-use-keyboxd" > "$GPG_LOCAL/common.conf"
    # Import public keys to legacy format
    GNUPGHOME="$GPG_LOCAL" gpg --import "$GPG_HOST/pubring.kbx" 2>/dev/null || \
    GNUPGHOME="$GPG_LOCAL" gpg --import "$GPG_HOST/pubring.gpg" 2>/dev/null || true
    # Configure agent
    cat > "$GPG_LOCAL/gpg-agent.conf" <<AGENT
pinentry-program /usr/bin/pinentry-curses
default-cache-ttl 86400
max-cache-ttl 86400
AGENT
fi
echo "=== GPG isolation complete ==="
