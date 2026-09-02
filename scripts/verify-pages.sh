#!/bin/bash
# Check that the repository's GitHub Pages site is enabled and serving the
# gh-pages branch.
#
# Usage: scripts/verify-pages.sh <owner/repo>
#
# Pushing gh-pages does not publish anything by itself. When the Pages site is
# disabled — switched off, or repointed at another source — the push still
# succeeds and every package URL answers 404, with nothing in the release log
# to say so. Provisioning then fails at `apt-get update` inside the VM, a long
# way from the release that broke it. Checking here turns that silent failure
# into a failed run.
#
# This only reports; it does not try to enable or repoint the site. Creating or
# changing a Pages source needs repository administration rights, which
# GITHUB_TOKEN does not carry even with `pages: write` — the API answers 403.
# cloister's site is already configured and serving gh-pages, so the only thing
# a write attempt could add here is a misleading 403 in the log on the one run
# where the check has something real to report. The remediation a human needs is
# printed instead.
#
# Environment:
#   GH_TOKEN — token for the GitHub CLI (required)

set -euo pipefail

REPO_SLUG="${1:?Usage: verify-pages.sh <owner/repo>}"

# Read through gh's own --jq so the result is plain text: gh pretty-prints and
# colorizes raw JSON when it believes it has a terminal, and those escape codes
# make a separate JSON parser fail on perfectly good output.
if CONFIG=$(gh api "repos/${REPO_SLUG}/pages" --jq '"\(.source.branch // "")\t\(.source.path // "")\t\(.status // "")"' 2>/dev/null); then
    BRANCH=$(printf '%s' "${CONFIG}" | cut -f1)
    SOURCE_PATH=$(printf '%s' "${CONFIG}" | cut -f2)
    STATUS=$(printf '%s' "${CONFIG}" | cut -f3)
    if [ "${BRANCH}" = "gh-pages" ] && [ "${SOURCE_PATH}" = "/" ]; then
        echo "Pages site is enabled and serving gh-pages (build status: ${STATUS:-unknown})"
        exit 0
    fi
    DIAGNOSIS="it serves ${BRANCH:-an unset branch}:${SOURCE_PATH:-/}"
else
    DIAGNOSIS="it is disabled, or this token cannot read its configuration"
fi

cat >&2 <<MESSAGE
::error::The GitHub Pages site for ${REPO_SLUG} is not serving gh-pages: ${DIAGNOSIS}.
The packages were pushed to the gh-pages branch, but nothing will serve them,
so every VM provision will fail to reach the cloister APT repository.

This token cannot fix it: creating or repointing a Pages site needs repository
administration rights that GITHUB_TOKEN does not have even with pages: write.
Someone with administration rights needs to run, once:

  echo '{"source":{"branch":"gh-pages","path":"/"}}' |
    gh api -X POST repos/${REPO_SLUG}/pages --input -

or set Settings -> Pages -> Source to the gh-pages branch, then re-run the
"Republish APT repository" workflow.
MESSAGE
exit 1
