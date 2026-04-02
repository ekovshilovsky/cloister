#!/bin/bash
# Build APT repository metadata for the cloister package pool.
# Scans pool/ for .deb files, generates per-architecture Packages indices,
# writes a Release file, and optionally GPG-signs it if GPG_KEY_ID is set.
#
# Usage: build-apt-repo.sh <repo-dir>
#   repo-dir - Root of the APT repository (must contain a pool/ subdirectory
#              with .deb files already copied into it)
#
# Environment variables:
#   GPG_KEY_ID - GPG key fingerprint or email used to sign the Release file.
#                If unset, the repository is left unsigned and users must add
#                [trusted=yes] to their sources.list entry.
set -euo pipefail

REPO_DIR="$(realpath "${1:?Usage: build-apt-repo.sh <repo-dir>}")"

mkdir -p "${REPO_DIR}/dists/stable/main/binary-amd64"
mkdir -p "${REPO_DIR}/dists/stable/main/binary-arm64"

for ARCH in amd64 arm64; do
    PACKAGES_DIR="${REPO_DIR}/dists/stable/main/binary-${ARCH}"
    cd "${REPO_DIR}"
    dpkg-scanpackages --arch "${ARCH}" pool/ > "${PACKAGES_DIR}/Packages"
    gzip -9 -k -f "${PACKAGES_DIR}/Packages"
    cd - > /dev/null
    echo "Generated: dists/stable/main/binary-${ARCH}/Packages"
done

cat > "${REPO_DIR}/dists/stable/Release" <<RELEASE
Origin: cloister
Label: cloister
Suite: stable
Codename: stable
Date: $(date -u '+%a, %d %b %Y %H:%M:%S UTC')
Architectures: amd64 arm64
Components: main
Description: cloister APT repository — in-VM toolkit for cloister-managed VMs
RELEASE

cd "${REPO_DIR}/dists/stable"
{
    echo "MD5Sum:"
    for f in main/binary-*/Packages main/binary-*/Packages.gz; do
        [ -f "$f" ] && printf " %s %s %s\n" "$(md5sum "$f" | cut -d' ' -f1)" "$(wc -c < "$f" | tr -d ' ')" "$f"
    done
    echo "SHA256:"
    for f in main/binary-*/Packages main/binary-*/Packages.gz; do
        [ -f "$f" ] && printf " %s %s %s\n" "$(sha256sum "$f" | cut -d' ' -f1)" "$(wc -c < "$f" | tr -d ' ')" "$f"
    done
} >> Release
cd - > /dev/null

if [ -n "${GPG_KEY_ID:-}" ]; then
    gpg --batch --yes --default-key "${GPG_KEY_ID}" --armor --detach-sign \
        --output "${REPO_DIR}/dists/stable/Release.gpg" "${REPO_DIR}/dists/stable/Release"
    gpg --batch --yes --default-key "${GPG_KEY_ID}" --armor --clearsign \
        --output "${REPO_DIR}/dists/stable/InRelease" "${REPO_DIR}/dists/stable/Release"
    gpg --armor --export "${GPG_KEY_ID}" > "${REPO_DIR}/key.gpg"
    echo "Signed Release and exported public key"
else
    echo "Warning: GPG_KEY_ID not set — Release file is unsigned"
    echo "Users will need [trusted=yes] in their sources.list"
fi

echo "APT repository metadata generated at ${REPO_DIR}"
