#!/usr/bin/env bash
# Publishes the IBM Cloud Virtual Server for VPC bundle (IBMVPC) to R2 as `ibm-latest` and `ibm-bundle-<timestamp>.json.xz`.
# other/update-ibm.sh adds the current prices to it. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" IBMVPC "ibm-" other/update-ibm.sh
