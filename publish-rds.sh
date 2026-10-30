#!/usr/bin/env bash
# Publishes the RDS bundle (AmazonRDS) to R2 as `rds-latest` and `rds-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonRDS "rds-"
