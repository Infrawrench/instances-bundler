#!/usr/bin/env bash
# Publishes the EC2 spot price bundle (AmazonEC2Spot) to R2 as `spot-latest` and `spot-bundle-<timestamp>.json.xz`.
# spot.sh updates it from AWS's spot price feed. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonEC2Spot "spot-" spot.sh
