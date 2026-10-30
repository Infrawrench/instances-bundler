#!/usr/bin/env bash
# Publishes the DocumentDB bundle (AmazonDocDB) to R2 as `docdb-latest` and `docdb-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonDocDB "docdb-"
