#!/bin/bash
# Check that a generated APT repository is fit to publish.
#
# Usage: scripts/verify-apt-repo.sh <repo-dir>
#
# An APT repository fails open. A Packages index naming no packages is a valid
# index, so `apt-get install cloister-vm` against it reports only that the
# package does not exist, and a publish that produced one succeeds with nothing
# in the log to distinguish it from a good release. Every VM provisioned after
# that point silently comes up without the toolkit.
#
# This turns those into a failed run before anything is pushed. It is run by
# both the release and republish workflows so that the two publish paths cannot
# drift into checking different things.
#
# Checks, in order of how badly each would break an installation:
#   1. Release/InRelease/Release.gpg/key.gpg exist and are non-empty. VMs add
#      this repository with signed-by=, so unsigned metadata is not degraded
#      service — apt rejects the repository outright.
#   2. Each architecture's index names at least one package.
#   3. Every Filename: the index advertises resolves to a real, non-empty file,
#      so a truncated clone or half-finished download cannot be published as a
#      complete pool.
#   4. The Packages checksums recorded in Release match the Packages files on
#      disk, which catches a Release left over from an earlier build.

set -euo pipefail

REPO_DIR="$(realpath "${1:?Usage: verify-apt-repo.sh <repo-dir>}")"
ARCHITECTURES=(amd64 arm64)

fail() {
    echo "::error::$*" >&2
    exit 1
}

for REQUIRED in \
    "dists/stable/Release" \
    "dists/stable/InRelease" \
    "dists/stable/Release.gpg" \
    "key.gpg"; do
    [ -s "${REPO_DIR}/${REQUIRED}" ] || fail "${REQUIRED} is missing or empty; refusing to publish"
done

RELEASE_FILE="${REPO_DIR}/dists/stable/Release"

for ARCH in "${ARCHITECTURES[@]}"; do
    PACKAGES="${REPO_DIR}/dists/stable/main/binary-${ARCH}/Packages"
    [ -s "${PACKAGES}" ] || fail "the ${ARCH} index is missing or empty; refusing to publish"
    [ -s "${PACKAGES}.gz" ] || fail "the ${ARCH} compressed index is missing or empty; refusing to publish"

    COUNT=$(grep -c '^Package: ' "${PACKAGES}" || true)
    [ "${COUNT}" -gt 0 ] || fail "the ${ARCH} index names no packages; refusing to publish"
    echo "${ARCH}: ${COUNT} package(s) indexed"

    while read -r FILENAME; do
        [ -n "${FILENAME}" ] || continue
        [ -s "${REPO_DIR}/${FILENAME}" ] ||
            fail "the ${ARCH} index advertises ${FILENAME}, which is missing or empty"
        echo "  ${FILENAME}"
    done < <(sed -n 's/^Filename: //p' "${PACKAGES}")

    # The indexes are regenerated on every build, so a Release that does not
    # describe them is a stale file rather than a corrupt one — apt reports it
    # as a hash mismatch and refuses the whole repository.
    for INDEX in "${PACKAGES}" "${PACKAGES}.gz"; do
        DIGEST=$(sha256sum "${INDEX}" | cut -d' ' -f1)
        grep -q " ${DIGEST} " "${RELEASE_FILE}" ||
            fail "Release does not record the current checksum of ${INDEX}; it is stale"
    done
done

echo "APT repository at ${REPO_DIR} verified"
