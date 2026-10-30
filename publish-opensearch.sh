#!/usr/bin/env bash
# Publishes the OpenSearch bundle (AmazonES) to R2 as `opensearch-latest` and `opensearch-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonES "opensearch-"
