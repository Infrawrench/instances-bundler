#!/usr/bin/env bash
# Publishes the Azure virtual machine bundle (AzureVM) to R2 as `azure-latest` and `azure-bundle-<timestamp>.json.xz`.
# other/update-azure.sh adds Azure's current prices to it. See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AzureVM "azure-" other/update-azure.sh
