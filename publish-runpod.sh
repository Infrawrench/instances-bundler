#!/usr/bin/env bash
# Publishes the RunPod bundle (RunpodPods) to R2 as `runpod-latest` and `runpod-bundle-<timestamp>.json.xz`.
# other/update-runpod.sh adds RunPod's current prices to it. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" RunpodPods "runpod-" other/update-runpod.sh
