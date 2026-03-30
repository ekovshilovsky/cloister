#!/bin/bash
set -euo pipefail
echo "=== Installing Python stack ==="
sudo apt-get install -y -qq make build-essential libssl-dev zlib1g-dev libbz2-dev libreadline-dev libsqlite3-dev llvm libncursesw5-dev xz-utils tk-dev libxml2-dev libxmlsec1-dev libffi-dev liblzma-dev
curl -fsSL -o /tmp/pyenv-install.sh https://pyenv.run
bash /tmp/pyenv-install.sh
rm -f /tmp/pyenv-install.sh
export PYENV_ROOT="$HOME/.pyenv"
export PATH="$PYENV_ROOT/bin:$PATH"
eval "$(pyenv init -)"
pyenv install -s 3  # latest Python 3
pyenv global 3
echo "=== Python stack complete ==="
