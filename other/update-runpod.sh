#!/usr/bin/env bash
# Adds RunPod's current GPU and CPU pod prices to a RunPod bundle, creating the bundle if it doesn't exist.
#
# RunPod publishes no history, only its current prices, so the bundle holds every price seen since its first
# run. A read that changes nothing is not recorded.
#
#   ./update-runpod.sh [runpod-bundle.json]
set -euo pipefail

bundle="${1:-runpod-bundle.json}"

command -v go >/dev/null || { echo "update-runpod: go is required" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/other-instances"
(cd "$here" && go build -o "$bin" .)

work="$(mktemp -d "${TMPDIR:-/tmp}/other-instances-runpod.XXXXXX")"
trap 'rm -rf "$work"' EXIT

"$bin" fetch runpod -o "$work/runpod.json"

status=0
if [[ -f "$bundle" ]]; then
  "$bin" append "$bundle" "$work/runpod.json" || status=$?
else
  "$bin" compress -o "$bundle" "$work/runpod.json" || status=$?
fi
if [[ "$status" -eq 3 ]]; then
  echo "update-runpod: prices have not changed since $bundle's latest snapshot" >&2
  exit 0
fi
exit "$status"
