#!/usr/bin/env bash
# Publishes the Google Cloud Compute Engine bundle (GCPCompute) to R2 as `gcp-latest` and `gcp-bundle-<timestamp>.json.xz`.
# other/update-gcp.sh adds Google's current prices to it. See publish-offer.sh for the environment it needs, and
# other/gcpauth.go for the GCP credentials (GCP_ACCESS_TOKEN and GCP_PROJECT in CI).
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" GCPCompute "gcp-" other/update-gcp.sh
