#!/usr/bin/env bash
# Adds IBM Cloud's current Virtual Server for VPC prices to a bundle, creating the bundle if it doesn't exist.
#
# IBM's catalog only has current prices, so the bundle holds every price seen since its first run.
# A read that changes nothing is not recorded.
#
#   ./update-ibm.sh [ibm-bundle.json]
set -euo pipefail

bundle="${1:-ibm-bundle.json}"

command -v go >/dev/null || { echo "update-ibm: go is required" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-ibm.XXXXXX")"
trap 'rm -rf "$work"' EXIT

"$bin" fetch ibm -o "$work/ibm.json"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/ibm.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/ibm.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "update-ibm: prices have not changed since $bundle's latest snapshot" >&2
  exit 0
fi
exit "$status"
