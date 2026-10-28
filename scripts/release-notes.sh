#!/bin/sh
# Prints one version's section of CHANGELOG.md, without its heading, for the
# GitHub release notes. Fails if the changelog has no section for it.
#
#   scripts/release-notes.sh 0.1.0
set -eu

version=${1:?usage: scripts/release-notes.sh VERSION}

notes=$(awk -v v="$version" '
  index($0, "## [" v "]") == 1 { found = 1; next }
  found && /^## \[/ { exit }
  found { print }
' CHANGELOG.md)

if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
  echo "release-notes: CHANGELOG.md has no section for $version" >&2
  exit 1
fi
printf '%s\n' "$notes" | sed -e '/./,$!d'
