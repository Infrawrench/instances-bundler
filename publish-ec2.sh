#!/usr/bin/env bash
# Publishes the EC2 bundle (AmazonEC2) to R2 as `latest` and `bundle-<timestamp>.json.xz`.
# These keys have no prefix because they were in use before the other platforms were added.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonEC2 ""
