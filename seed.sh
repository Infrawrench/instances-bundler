#!/usr/bin/env bash
# Builds a bundle from every published version of an AWS offer's price list (EC2 unless OFFER says
# otherwise), oldest first: the first file creates the bundle and every later one is appended.
#
# Re-running resumes: versions already in the bundle are skipped. Each file is downloaded to a
# temp dir (up to ~9 GB for recent EC2 versions), applied, then deleted. A full EC2 seed downloads
# ~400 GB; the other offers' files are at most a few hundred MB each.
#
#   ./seed.sh [bundle.json]
#
# Environment:
#   MAX_VERSIONS  stop after adding this many versions (default 0 = all)
#   HOST_SUFFIX   suffix for Dedicated Host names (default .host); keep it the same for a bundle
#   OFFER         AWS offer code (default AmazonEC2); the bundle must be for the same offer
#   TMPDIR        where downloads go while they are applied
set -euo pipefail

bundle="${1:-bundle.json}"
offer="${OFFER:-AmazonEC2}"
host_suffix="${HOST_SUFFIX:-.host}"
max_versions="${MAX_VERSIONS:-0}"
pricing="https://pricing.us-east-1.amazonaws.com"

for cmd in curl jq go; do
  command -v "$cmd" >/dev/null || { echo "seed: $cmd is required" >&2; exit 1; }
done

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/instances-bundler"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/instances-seed.XXXXXX")"
trap 'rm -rf "$work"' EXIT

curl -fsSL --retry 5 "$pricing/offers/v1.0/aws/$offer/index.json" -o "$work/versions.json"

# Version ids are usually the file's publicationDate (20200924194735 = 2020-09-24T19:47:35Z), but not
# always: 20190828220000 was published at 2019-08-28T20:49:02Z. Ids well before the bundle's newest
# snapshot are skipped outright; ones within a week of it are checked against the real publicationDate.
last=0
if [[ -f "$bundle" ]]; then
  last="$(jq '.snapshots[-1] // 0' "$bundle")"
fi
jq -r --argjson last "$last" '
  .versions | to_entries
  | map({id: .key, url: .value.offerVersionUrl, ts: (.key | strptime("%Y%m%d%H%M%S") | mktime)})
  | sort_by(.id)[] | select(.ts > $last - 604800) | "\(.id) \(.url)"
' "$work/versions.json" > "$work/todo"

total="$(wc -l < "$work/todo" | tr -d ' ')"
echo "seed: $total candidate version(s) for $bundle" >&2

# publication_ts prints a file's publicationDate as Unix seconds, read from its first few KB.
publication_ts() {
  local pub
  pub="$(curl -fsS -r 0-4095 "$1" < /dev/null | grep -o '"publicationDate" *: *"[^"]*"' | cut -d'"' -f4 || true)"
  [[ -n "$pub" ]] && jq -rn --arg d "$pub" '$d | fromdateiso8601'
}

progress=(-sS)
[[ -t 2 ]] && progress=(--progress-bar)

n=0
added=0
check_published=$([[ "$last" -gt 0 ]] && echo 1 || echo 0)
while read -r id url; do
  if [[ "$max_versions" -gt 0 && "$added" -ge "$max_versions" ]]; then
    break
  fi
  n=$((n + 1))
  if [[ "$check_published" -eq 1 ]]; then
    pub_ts="$(publication_ts "$pricing$url" || true)"
    if [[ -n "$pub_ts" && "$pub_ts" -le "$last" ]]; then
      echo "seed: [$n/$total] $id is already in $bundle, skipping" >&2
      continue
    fi
  fi
  file="$work/$id.json"
  echo "seed: [$n/$total] downloading $id" >&2
  curl -fL "${progress[@]}" --retry 5 --retry-all-errors --retry-delay 10 -C - -o "$file" "$pricing$url" < /dev/null
  status=0
  if [[ -f "$bundle" ]]; then
    "$bin" append -host-suffix "$host_suffix" "$bundle" "$file" < /dev/null || status=$?
  else
    "$bin" compress -host-suffix "$host_suffix" -o "$bundle" "$file" < /dev/null || status=$?
  fi
  rm -f "$file"
  if [[ "$status" -eq 3 ]]; then
    echo "seed: $id is not newer than $bundle's latest version, skipping" >&2
    continue
  fi
  [[ "$status" -eq 0 ]] || exit "$status"
  added=$((added + 1))
  # Everything after this in the list was published later than what was just added.
  check_published=0
done < "$work/todo"

echo "seed: done, $bundle has $(jq '.snapshots | length' "$bundle") version(s)" >&2
