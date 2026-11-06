#!/usr/bin/env bash
# Adds Hetzner's current cloud server prices to a bundle, creating the bundle if it doesn't exist.
#
# Hetzner publishes no history, only its current prices, so the bundle holds every price seen since its first run.
# A read that changes nothing is not recorded.
#
#   ./update-hetzner.sh [hetzner-bundle.json]
set -euo pipefail

bundle="${1:-hetzner-bundle.json}"

command -v go >/dev/null || { echo "update-hetzner: go is required" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-hetzner.XXXXXX")"
trap 'rm -rf "$work"' EXIT

"$bin" fetch hetzner -o "$work/hetzner.json"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/hetzner.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/hetzner.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "update-hetzner: prices have not changed since $bundle's latest snapshot" >&2
  exit 0
fi
exit "$status"
