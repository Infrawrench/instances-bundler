#!/usr/bin/env bash
# Publishes the Oracle Cloud compute bundle (OracleCompute) to R2 as `oracle-latest` and `oracle-bundle-<timestamp>.json.xz`.
# other/update-oracle.sh adds the current prices to it. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" OracleCompute "oracle-" other/update-oracle.sh
