#!/bin/bash
set -euo pipefail

# Source NVM so npm/node are available (installed by base provisioning)
export NVM_DIR="$HOME/.nvm"
[ -s "$NVM_DIR/nvm.sh" ] && source "$NVM_DIR/nvm.sh"

echo "=== Installing web stack ==="

# Playwright with its bundled Chromium build. The distro chromium-browser
# package is intentionally avoided: on Ubuntu 22.04+ it is a transitional
# .deb that installs the snap, whose AppArmor profile whitelists only the
# guest's $HOME, /tmp, and $SNAP. Any Chrome read against the VirtioFS-shared
# host workspace (paths under /Users/... inside the VM) is denied, which
# breaks mermaid-cli, Marp, and other Puppeteer-driven renderers that hand
# Chrome a project-side path. Playwright's tarball has no AppArmor profile
# and inherits the user's POSIX read permissions on the shared mount.
npm install -g playwright
npx playwright install-deps chromium   # installs GTK/NSS/font runtime libs via apt
npx playwright install chromium        # downloads Chromium to ~/.cache/ms-playwright

# Publish the bundled binary at the conventional discovery paths so that
# Puppeteer-based tools find it without per-project configuration. A wrapper
# script (rather than a static symlink) resolves the active Chromium at
# invocation time, so Playwright version bumps do not invalidate the path
# by changing the versioned chromium-XXXX directory name.
sudo mkdir -p /opt/google/chrome
sudo tee /opt/google/chrome/chrome >/dev/null <<'WRAPPER'
#!/bin/bash
chrome=$(ls -td "${HOME}"/.cache/ms-playwright/chromium-*/chrome-linux/chrome 2>/dev/null | head -1)
if [[ -z "$chrome" ]]; then
  echo "Playwright-managed Chromium not found under ${HOME}/.cache/ms-playwright; run 'npx playwright install chromium' first." >&2
  exit 127
fi
exec "$chrome" "$@"
WRAPPER
sudo chmod +x /opt/google/chrome/chrome
sudo ln -sf /opt/google/chrome/chrome /usr/local/bin/chromium-browser

# GitHub CLI
(type gh >/dev/null 2>&1) || {
  curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null
  sudo apt-get update -q && sudo apt-get install -y -q gh
}
# Vercel CLI
npm install -g vercel
echo "=== Web stack complete ==="
