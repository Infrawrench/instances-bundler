#!/usr/bin/env bash
# Publishes the OVHcloud eco dedicated server bundle (OVHEco) to R2 as `ovheco-latest` and `ovheco-bundle-<timestamp>.json.xz`.
# other/update-ovheco.sh adds the current prices to it. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" OVHEco "ovheco-" other/update-ovheco.sh
