#!/usr/bin/env bash
# Publishes the Alibaba Cloud ECS bundle (AlibabaECS) to R2 as `alibaba-latest` and `alibaba-bundle-<timestamp>.json.xz`.
# other/update-alibaba.sh adds every price version published since. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AlibabaECS "alibaba-" other/update-alibaba.sh
