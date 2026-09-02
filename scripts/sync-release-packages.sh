#!/bin/bash
# Populate the APT pool from the .deb packages attached to cloister's GitHub
# releases.
#
# Usage: scripts/sync-release-packages.sh <repo-dir> [repo-slug]
#
# Expects to write .deb files into <repo-dir>/pool/
#
# The gh-pages branch is a cache, not the record. Every package it serves is
# also attached to the release that produced it, so the published repository
# can be rebuilt from the releases alone. That is what makes it recoverable: a
# gh-pages branch that is deleted, truncated, or never created is repaired by
# running this and regenerating the indexes, without cutting a version whose
# only purpose is to trigger a publish.
#
# Releases up to and including v0.16.1 predate the change that attaches .deb
# packages, and carry only the macOS tarballs. Those versions are therefore NOT
# recoverable from the releases and are skipped here; the pool can only be
# rebuilt from v0.16.2 onward. Recovering an older cloister-vm build means
# rebuilding it from its tag with scripts/build-deb-vm.sh.
#
# Existing files are left alone, so this is safe to run against a populated
# pool and cheap when nothing is missing.
#
# Environment:
#   GH_TOKEN  — token for the GitHub CLI (required)

set -euo pipefail

REPO_DIR="${1:?Usage: sync-release-packages.sh <repo-dir> [repo-slug]}"
REPO_SLUG="${2:-${GITHUB_REPOSITORY:-ekovshilovsky/cloister}}"

POOL_DIR="${REPO_DIR}/pool"
mkdir -p "${POOL_DIR}"

STAGE="$(mktemp -d)"
trap 'rm -rf "${STAGE}"' EXIT

downloaded=0
present=0
for TAG in $(gh release list --repo "${REPO_SLUG}" --limit 200 --json tagName --jq '.[].tagName'); do
    for ASSET in $(gh release view "${TAG}" --repo "${REPO_SLUG}" --json assets --jq '.assets[].name'); do
        case "${ASSET}" in
            *.deb) ;;
            # Releases also carry the macOS tarballs, which belong on the
            # release page and not in an APT pool.
            *) continue ;;
        esac
        if [ -f "${POOL_DIR}/${ASSET}" ]; then
            present=$((present + 1))
            continue
        fi
        # gh writes into the current directory, so each download lands in a
        # scratch area first and is moved into place only once it is complete.
        # A partial .deb left in the pool would be indexed as if it were whole.
        rm -rf "${STAGE:?}/dl"
        mkdir -p "${STAGE}/dl"
        if ! (cd "${STAGE}/dl" && gh release download "${TAG}" --repo "${REPO_SLUG}" --pattern "${ASSET}" >/dev/null 2>&1); then
            echo "  warning: could not download ${ASSET} from ${TAG}"
            continue
        fi
        mv "${STAGE}/dl/${ASSET}" "${POOL_DIR}/${ASSET}"
        echo "  recovered pool/${ASSET} from ${TAG}"
        downloaded=$((downloaded + 1))
    done
done

echo "Package sync complete: ${downloaded} recovered, ${present} already present"
