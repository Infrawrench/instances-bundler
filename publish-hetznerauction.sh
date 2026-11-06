#!/usr/bin/env bash
# Publishes the Hetzner Server Auction bundle (HetznerAuction) to R2 as `hetznerauction-latest` and `hetznerauction-bundle-<timestamp>.json.xz`.
# other/update-hetznerauction.sh adds the current prices to it. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" HetznerAuction "hetznerauction-" other/update-hetznerauction.sh
