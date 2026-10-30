#!/usr/bin/env bash
# Publishes the GameLift bundle (AmazonGameLift) to R2 as `gamelift-latest` and `gamelift-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonGameLift "gamelift-"
