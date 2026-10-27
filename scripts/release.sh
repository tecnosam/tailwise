#!/bin/sh
# Cuts a release: the next semantic version, its CHANGELOG.md section, the Helm
# chart's version, a commit and an annotated tag. Nothing is pushed.
#
#   scripts/release.sh [auto|patch|minor|major]     (or: make release BUMP=...)
#
# auto, the default, picks the bump from the Conventional Commits since the
# last tag (see cliff.toml). The first release is 0.1.0.
set -eu

bump=${1:-auto}
cliff=${GIT_CLIFF:-npx --yes git-cliff@2.14.2}
chart=deploy/helm/tailwise/Chart.yaml

case $bump in auto | patch | minor | major) ;; *)
  echo "release: BUMP must be auto, patch, minor or major, not '$bump'" >&2
  exit 2
  ;;
esac
if [ -n "$(git status --porcelain)" ]; then
  echo "release: commit or stash your changes first" >&2
  exit 1
fi

if [ -z "$(git tag -l 'v[0-9]*')" ]; then
  # CHANGELOG.md already describes the first release.
  next=0.1.0
else
  next=$($cliff --bumped-version --bump "$bump" | sed 's/^v//')
  if git rev-parse -q --verify "refs/tags/v$next" >/dev/null; then
    echo "release: v$next already exists; nothing to release since it" >&2
    exit 1
  fi
  # The new section goes right below the header, above the last release.
  section=$($cliff --unreleased --bump "$bump" --strip all)
  printf '%s\n\n' "$section" > .release-section.md
  awk 'FNR == NR { s = s $0 "\n"; next }
       !done && /^## \[/ { printf "%s", s; done = 1 }
       { print }
       END { if (!done) printf "\n%s", s }' .release-section.md CHANGELOG.md > .release-changelog.md
  mv .release-changelog.md CHANGELOG.md
  rm -f .release-section.md
fi

sed -i.bak -e "s/^version: .*/version: $next/" -e "s/^appVersion: .*/appVersion: \"$next\"/" "$chart"
rm -f "$chart.bak"

git add CHANGELOG.md "$chart"
git diff --cached --quiet || git commit -q -m "chore(release): v$next"
git tag -a "v$next" -m "Tailwise v$next"
echo "Tagged v$next. Push it with: git push --follow-tags"
