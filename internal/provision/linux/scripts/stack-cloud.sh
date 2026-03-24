#!/bin/bash
set -euo pipefail
echo "=== Installing cloud stack ==="
# AWS CLI v2
if ! command -v aws &>/dev/null; then
  curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip" -o /tmp/awscli.zip
  cd /tmp && unzip -qo awscli.zip && sudo ./aws/install && rm -rf aws awscli.zip
fi
# Terraform
if ! command -v terraform &>/dev/null; then
  sudo apt-get install -y -qq gnupg software-properties-common
  wget -qO- https://apt.releases.hashicorp.com/gpg | gpg --dearmor | sudo tee /usr/share/keyrings/hashicorp-archive-keyring.gpg >/dev/null
  echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" | sudo tee /etc/apt/sources.list.d/hashicorp.list
  sudo apt-get update -qq && sudo apt-get install -y -qq terraform
fi
echo "=== Cloud stack complete ==="
