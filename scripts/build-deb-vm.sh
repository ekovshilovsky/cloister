#!/bin/bash
# Build .deb packages for cloister-vm for amd64 and arm64 architectures.
# Each package installs the cloister-vm binary to /usr/local/bin.
#
# Usage: build-deb-vm.sh <version> <binary-dir> <output-dir>
#   version     - Package version string (e.g. "0.1.0")
#   binary-dir  - Directory containing cloister-vm_<version>_linux_<arch>/ subdirectories
#   output-dir  - Directory where .deb files will be written
set -euo pipefail

VERSION="${1:?Usage: build-deb-vm.sh <version> <binary-dir> <output-dir>}"
BINARY_DIR="${2:?}"
OUTPUT_DIR="${3:?}"

mkdir -p "$OUTPUT_DIR"

for ARCH in amd64 arm64; do
    DEB_ARCH="$ARCH"
    SRC_DIR="${BINARY_DIR}/cloister-vm_${VERSION}_linux_${ARCH}"
    PKG_DIR=$(mktemp -d)

    mkdir -p "${PKG_DIR}/usr/local/bin"
    mkdir -p "${PKG_DIR}/DEBIAN"

    cp "${SRC_DIR}/cloister-vm" "${PKG_DIR}/usr/local/bin/cloister-vm"
    chmod 755 "${PKG_DIR}/usr/local/bin/cloister-vm"

    cat > "${PKG_DIR}/DEBIAN/control" <<CTRL
Package: cloister-vm
Version: ${VERSION}
Section: utils
Priority: optional
Architecture: ${DEB_ARCH}
Maintainer: Eugene Kovshilovsky <ekovshilovsky@users.noreply.github.com>
Homepage: https://github.com/ekovshilovsky/cloister
Description: In-VM toolkit CLI for cloister-managed virtual machines
 cloister-vm provides status, diagnostics, and configuration tools
 for use inside cloister-managed virtual machines. It exposes tunnel
 health checks, Ollama model listing, Claude Code mode switching, and
 a doctor command for verifying the VM environment configuration.
CTRL

    DEB_FILE="${OUTPUT_DIR}/cloister-vm_${VERSION}_${DEB_ARCH}.deb"
    dpkg-deb --build --root-owner-group "${PKG_DIR}" "${DEB_FILE}"

    rm -rf "${PKG_DIR}"
    echo "Built: ${DEB_FILE}"
done
