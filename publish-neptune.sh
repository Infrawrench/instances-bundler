#!/usr/bin/env bash
# Publishes the Neptune bundle (AmazonNeptune) to R2 as `neptune-latest` and `neptune-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonNeptune "neptune-"
