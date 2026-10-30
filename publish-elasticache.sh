#!/usr/bin/env bash
# Publishes the ElastiCache bundle (AmazonElastiCache) to R2 as `elasticache-latest` and `elasticache-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonElastiCache "elasticache-"
