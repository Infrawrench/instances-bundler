#!/usr/bin/env bash
# Publishes the Hetzner cloud server bundle (HetznerCloud) to R2 as `hetzner-latest` and `hetzner-bundle-<timestamp>.json.xz`.
# other/update-hetzner.sh adds the current prices to it. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" HetznerCloud "hetzner-" other/update-hetzner.sh
