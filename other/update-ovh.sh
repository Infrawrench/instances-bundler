#!/usr/bin/env bash
# Adds OVHcloud's current Public Cloud instance prices to a bundle, creating the bundle if it doesn't exist.
#
# OVHcloud publishes no history, only its current catalog, so the bundle holds every price seen since its first run.
# A read that changes nothing is not recorded.
#
#   ./update-ovh.sh [ovh-bundle.json]
set -euo pipefail

bundle="${1:-ovh-bundle.json}"

command -v go >/dev/null || { echo "update-ovh: go is required" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-ovh.XXXXXX")"
trap 'rm -rf "$work"' EXIT

"$bin" fetch ovh -o "$work/ovh.json"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/ovh.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/ovh.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "update-ovh: prices have not changed since $bundle's latest snapshot" >&2
  exit 0
fi
exit "$status"
