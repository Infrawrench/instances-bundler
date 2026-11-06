#!/usr/bin/env bash
# Adds the Hetzner Server Auction's current listings to a bundle, creating the bundle if it doesn't exist.
#
# The auction only shows what is for sale now, so a server's history runs from when a read first saw it until it is sold.
# A read that changes nothing is not recorded.
#
#   ./update-hetznerauction.sh [hetznerauction-bundle.json]
set -euo pipefail

bundle="${1:-hetznerauction-bundle.json}"

command -v go >/dev/null || { echo "update-hetznerauction: go is required" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-hetznerauction.XXXXXX")"
trap 'rm -rf "$work"' EXIT

"$bin" fetch hetznerauction -o "$work/hetznerauction.json"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/hetznerauction.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/hetznerauction.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "update-hetznerauction: prices have not changed since $bundle's latest snapshot" >&2
  exit 0
fi
exit "$status"
