#!/usr/bin/env bash
# Publishes the OVHcloud Public Cloud bundle (OVHPublicCloud) to R2 as `ovh-latest` and `ovh-bundle-<timestamp>.json.xz`.
# other/update-ovh.sh adds the current prices to it. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" OVHPublicCloud "ovh-" other/update-ovh.sh
