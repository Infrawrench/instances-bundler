#!/usr/bin/env bash
# Adds the current EC2 spot prices to a spot bundle, creating the bundle if it doesn't exist.
#
# AWS publishes no history of spot prices without an AWS account, only the current price per region and
# operating system in the feed behind its Spot pricing page. So a spot bundle holds the prices from every
# time this ran and saw a new feed, starting from its first run.
#
#   ./spot.sh [spot-bundle.json]
#
# Environment:
#   TMPDIR  where the feed (about 3 MB) is downloaded
set -euo pipefail

bundle="${1:-spot-bundle.json}"
feed="https://website.spot.ec2.aws.a2z.com/spot.json"

for cmd in curl go; do
  command -v "$cmd" >/dev/null || { echo "spot: $cmd is required" >&2; exit 1; }
done

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/instances-bundler"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/instances-spot.XXXXXX")"
trap 'rm -rf "$work"' EXIT

# -R dates the file by the feed's Last-Modified header, which instances-bundler records as its publication time.
curl -fsSL -R --retry 5 --retry-all-errors --retry-delay 10 -o "$work/spot.json" "$feed"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/spot.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/spot.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "spot: the feed has not changed since $bundle's latest version" >&2
  exit 0
fi
exit "$status"
