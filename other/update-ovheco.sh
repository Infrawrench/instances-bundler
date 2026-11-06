#!/usr/bin/env bash
# Adds OVHcloud's current Kimsufi, So you Start, Rise and Advance server prices to a bundle, creating the bundle if it doesn't exist.
#
# OVHcloud publishes no history, only its current catalog, so the bundle holds every price seen since its first run.
# A read that changes nothing is not recorded.
#
#   ./update-ovheco.sh [ovheco-bundle.json]
set -euo pipefail

bundle="${1:-ovheco-bundle.json}"

command -v go >/dev/null || { echo "update-ovheco: go is required" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-ovheco.XXXXXX")"
trap 'rm -rf "$work"' EXIT

"$bin" fetch ovheco -o "$work/ovheco.json"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/ovheco.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/ovheco.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "update-ovheco: prices have not changed since $bundle's latest snapshot" >&2
  exit 0
fi
exit "$status"
