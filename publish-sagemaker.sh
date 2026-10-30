#!/usr/bin/env bash
# Publishes the SageMaker bundle (AmazonSageMaker) to R2 as `sagemaker-latest` and `sagemaker-bundle-<timestamp>.json.xz`.
# See publish-offer.sh for the environment it needs.
exec "$(dirname "${BASH_SOURCE[0]}")/publish-offer.sh" AmazonSageMaker "sagemaker-"
