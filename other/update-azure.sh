#!/usr/bin/env bash
# Adds Azure's current virtual machine prices to an Azure bundle, creating the bundle if it doesn't exist.
#
# Azure publishes no price history, only its current prices (about 540,000 of them, read in about five minutes),
# so the bundle holds every price seen since its first run. A read that changes nothing is not recorded.
#
#   ./update-azure.sh [azure-bundle.json]
set -euo pipefail

bundle="${1:-azure-bundle.json}"

command -v go >/dev/null || { echo "update-azure: go is required" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-azure.XXXXXX")"
trap 'rm -rf "$work"' EXIT

"$bin" fetch azure -o "$work/azure.json"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/azure.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/azure.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "update-azure: prices have not changed since $bundle's latest snapshot" >&2
  exit 0
fi
exit "$status"
