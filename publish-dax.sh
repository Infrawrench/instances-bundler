#!/usr/bin/env bash
# Publishes the DAX bundle (AmazonDAX) to R2 as `dax-latest` and `dax-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonDAX "dax-"
