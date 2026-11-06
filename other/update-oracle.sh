#!/usr/bin/env bash
# Adds Oracle Cloud's current compute prices to a bundle, creating the bundle if it doesn't exist.
#
# Oracle publishes no history, only its current prices, so the bundle holds every price seen since its first run.
# A read that changes nothing is not recorded.
#
#   ./update-oracle.sh [oracle-bundle.json]
set -euo pipefail

bundle="${1:-oracle-bundle.json}"

command -v go >/dev/null || { echo "update-oracle: go is required" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-oracle.XXXXXX")"
trap 'rm -rf "$work"' EXIT

"$bin" fetch oracle -o "$work/oracle.json"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/oracle.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/oracle.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "update-oracle: prices have not changed since $bundle's latest snapshot" >&2
  exit 0
fi
exit "$status"
