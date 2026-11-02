#!/bin/sh
# Prints the CHANGELOG.md entry of a version (v0.4.0 or 0.4.0) or of "Unreleased": the notes of a release.
# Fails when there is no entry or when it is empty, so a tag without release notes does not release.
#
#   scripts/release-notes.sh v0.4.0
#   CHANGELOG=/path/to/CHANGELOG.md scripts/release-notes.sh Unreleased
set -eu

want=${1:?usage: release-notes.sh VERSION|Unreleased}
want=${want#v}
file=${CHANGELOG:-$(dirname "$0")/../CHANGELOG.md}

awk -v want="$want" '
  # The next version heading, or the link definitions at the end of the file, close the entry.
  /^## \[/ {
    if (on) exit
    h = $0; sub(/^## \[/, "", h); sub(/\].*/, "", h)
    on = (h == want)
    next
  }
  /^\[[^]]+\]: / { if (on) exit; next }
  on {
    if (n == 0 && $0 ~ /^[[:space:]]*$/) next
    lines[n++] = $0
  }
  END {
    while (n > 0 && lines[n - 1] ~ /^[[:space:]]*$/) n--
    for (i = 0; i < n; i++) print lines[i]
    if (n == 0) exit 3
  }
' "$file" || {
  echo "release-notes: no entry for \"$want\" in $file" >&2
  exit 1
}
