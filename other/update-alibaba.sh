#!/usr/bin/env bash
# Adds every Alibaba Cloud ECS price version published since an Alibaba bundle's latest one, oldest first,
# creating the bundle from the very first version (2.0.0, May 2018) if it doesn't exist.
#
# The bundle records the latest version it holds as source_version, so re-running resumes where it stopped.
# Each version is about 30 MB (recent ones), downloaded to a temp dir, applied, then deleted; a full seed is
# about 400 versions.
#
#   ./update-alibaba.sh [alibaba-bundle.json]
#
# Environment:
#   MAX_VERSIONS  stop after adding this many versions (default 0 = all)
#   TMPDIR        where downloads go while they are applied
set -euo pipefail

bundle="${1:-alibaba-bundle.json}"
max_versions="${MAX_VERSIONS:-0}"

for cmd in go jq; do
  command -v "$cmd" >/dev/null || { echo "update-alibaba: $cmd is required" >&2; exit 1; }
done

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-alibaba.XXXXXX")"
trap 'rm -rf "$work"' EXIT

after=""
if [[ -f "$bundle" ]]; then
  after="$(jq -r '.source_version // empty' "$bundle")"
  if [[ -z "$after" ]]; then
    echo "update-alibaba: $bundle has no source_version, so it can't tell which versions it has" >&2
    exit 1
  fi
fi
"$bin" versions alibaba ${after:+-after "$after"} > "$work/todo"
total="$(wc -l < "$work/todo" | tr -d ' ')"
echo "update-alibaba: $total version(s) after ${after:-the start} for $bundle" >&2

n=0
added=0
while read -r version; do
  if [[ "$max_versions" -gt 0 && "$added" -ge "$max_versions" ]]; then
    break
  fi
  n=$((n + 1))
  if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "update-alibaba: bad version '$version'" >&2
    exit 1
  fi
  src="$work/$version"
  echo "update-alibaba: [$n/$total] $version" >&2
  "$bin" fetch alibaba -version "$version" -o "$src" < /dev/null
  status=0
  if [[ -f "$bundle" ]]; then
    "$bin" append "$bundle" "$src" < /dev/null || status=$?
  else
    "$bin" compress -o "$bundle" "$src" < /dev/null || status=$?
  fi
  rm -rf "$src"
  if [[ "$status" -eq 3 ]]; then
    echo "update-alibaba: $version is not newer than $bundle's latest version, skipping" >&2
    continue
  fi
  [[ "$status" -eq 0 ]] || exit "$status"
  added=$((added + 1))
done < "$work/todo"

if [[ -f "$bundle" ]]; then
  echo "update-alibaba: done, $bundle has $(jq '.snapshots | length' "$bundle") version(s), latest $(jq -r .source_version "$bundle")" >&2
fi
