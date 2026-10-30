#!/usr/bin/env bash
# Publishes the MemoryDB bundle (AmazonMemoryDB) to R2 as `memorydb-latest` and `memorydb-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonMemoryDB "memorydb-"
