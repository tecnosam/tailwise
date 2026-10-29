#!/bin/sh
# Syncs issues/*.md to GitHub issues: each file is one issue, created the first
# time and edited after. An issue whose file was deleted is closed. Run by
# .github/workflows/issues.yml on pushes to main.
#
#   scripts/sync-issues.sh            (needs gh, authenticated, and jq)
#   DRY_RUN=1 scripts/sync-issues.sh  prints what it would do
#
# A file's first "# " line is the issue title, an optional "**Labels:** a, b"
# line its labels, and the rest its body. The body ends with a hidden marker
# naming the file, which is how a file finds its issue again.
set -eu

dir=${ISSUES_DIR:-issues}
dry=${DRY_RUN:-}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

run() {
  if [ -n "$dry" ]; then
    echo "would run: $*"
  else
    "$@"
  fi
}

marker() { printf '<!-- tailwise-issue: %s -->' "$1"; }

# Every issue, open or closed, with the file its marker names ("" for none).
# Fetched to a file first, so a failed fetch stops the script instead of
# looking like a repository with no issues.
gh issue list --state all --limit 1000 --json number,title,body,state > "$tmp/list.json"
jq '[.[] | . + {file: ((.body | capture("<!-- tailwise-issue: (?<f>[^ ]+) -->").f) // "")}]' "$tmp/list.json" > "$tmp/issues.json"

for file in "$dir"/*.md; do
  [ -e "$file" ] || continue
  title=$(sed -n 's/^# //p' "$file" | head -n 1)
  if [ -z "$title" ]; then
    echo "skip $file: no '# ' title line" >&2
    continue
  fi
  labels=$(sed -n 's/^\*\*Labels:\*\* *//p' "$file" | head -n 1)

  # The body: everything but the title and labels lines, leading blank lines
  # trimmed, then the marker.
  awk -v t="# $title" '
    !titled && $0 == t { titled = 1; next }
    /^\*\*Labels:\*\*/ && !labelled { labelled = 1; next }
    { print }
  ' "$file" | sed -e '/./,$!d' > "$tmp/body.md"
  printf '\n%s\n' "$(marker "$file")" >> "$tmp/body.md"

  echo "$labels" | tr ',' '\n' | sed 's/^ *//; s/ *$//' | sed '/^$/d' > "$tmp/labels"

  number=$(jq -r --arg f "$file" '[.[] | select(.file == $f)][0].number // empty' "$tmp/issues.json")
  if [ -z "$number" ]; then
    echo "create: $title ($file)"
    if [ -n "$dry" ]; then
      number="(new)"
    else
      number=$(gh issue create --title "$title" --body-file "$tmp/body.md" | sed 's|.*/||')
    fi
  else
    same=$(jq -r --arg f "$file" --arg t "$title" --rawfile b "$tmp/body.md" \
      '[.[] | select(.file == $f)][0] | (.title == $t and (.body | rtrimstr("\n")) == ($b | rtrimstr("\n")))' "$tmp/issues.json")
    if [ "$same" = true ]; then
      echo "unchanged: #$number $title"
    else
      echo "edit: #$number $title"
      run gh issue edit "$number" --title "$title" --body-file "$tmp/body.md"
    fi
  fi

  # Labels are only added: one removed from the file stays on the issue.
  while IFS= read -r l; do
    run gh label create "$l" --force </dev/null >/dev/null
    run gh issue edit "$number" --add-label "$l" </dev/null >/dev/null
  done < "$tmp/labels"
done

# Close open issues whose file is gone.
jq -r '.[] | select(.file != "" and .state == "OPEN") | "\(.number) \(.file)"' "$tmp/issues.json" |
  while read -r number file; do
    [ -e "$file" ] && continue
    echo "close: #$number ($file was deleted)"
    run gh issue close "$number" --comment "Closed because $file was removed from the repository." </dev/null
  done
