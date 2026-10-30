#!/usr/bin/env bash
# Publishes the Redshift bundle (AmazonRedshift) to R2 as `redshift-latest` and `redshift-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonRedshift "redshift-"
