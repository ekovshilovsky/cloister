#!/bin/bash
set -euo pipefail
echo "=== Installing Python stack ==="
sudo apt-get install -y -q make build-essential libssl-dev zlib1g-dev libbz2-dev libreadline-dev libsqlite3-dev llvm libncursesw5-dev xz-utils tk-dev libxml2-dev libxmlsec1-dev libffi-dev liblzma-dev

export PYENV_ROOT="$HOME/.pyenv"
if [ -d "$PYENV_ROOT" ]; then
    echo "pyenv already installed, updating..."
    cd "$PYENV_ROOT" && git pull --quiet 2>/dev/null || true
else
    curl -fsSL -o /tmp/pyenv-install.sh https://pyenv.run
    bash /tmp/pyenv-install.sh
    rm -f /tmp/pyenv-install.sh
fi

export PATH="$PYENV_ROOT/bin:$PATH"
eval "$(pyenv init -)"
pyenv install -s 3  # latest Python 3
pyenv global 3
echo "=== Python stack complete ==="
