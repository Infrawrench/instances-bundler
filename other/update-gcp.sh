#!/usr/bin/env bash
# Adds Google Cloud's current Compute Engine machine type prices to a GCP bundle, creating the bundle if it doesn't exist.
#
# Google publishes no price history, only its current prices, so the bundle holds every price seen since its first
# run. A read that changes nothing is not recorded. Reading them needs GCP credentials; see other/gcpauth.go.
#
#   ./update-gcp.sh [gcp-bundle.json]
set -euo pipefail

bundle="${1:-gcp-bundle.json}"

command -v go >/dev/null || { echo "update-gcp: go is required" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-gcp.XXXXXX")"
trap 'rm -rf "$work"' EXIT

"$bin" fetch gcp -o "$work/gcp.json"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/gcp.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/gcp.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "update-gcp: prices have not changed since $bundle's latest snapshot" >&2
  exit 0
fi
exit "$status"
