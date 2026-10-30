#!/usr/bin/env bash
# Updates the bundle for one AWS offer stored in R2 with any price list versions published since it was built.
# The publish-<platform>.sh scripts call this with their offer and prefix.
#
# The bucket holds `<prefix>latest`, whose contents are a timestamp, and `<prefix>bundle-<timestamp>.json.xz`
# for each published bundle. This downloads the bundle `<prefix>latest` points at, runs seed.sh on it, and if
# seed.sh changed it uploads it as <prefix>bundle-<now>.json.xz and then points `<prefix>latest` at <now>.
# When the bucket has no `<prefix>latest` yet, seed.sh builds a bundle from scratch (a full EC2 seed
# downloads ~400 GB; the other offers are far smaller).
#
#   R2_BUCKET=my-bucket R2_ACCOUNT_ID=abc123 ./publish-offer.sh AmazonRDS rds-
#
# Arguments:
#   offer   AWS offer code, such as AmazonEC2 or AmazonRDS
#   prefix  put before every key in the bucket; empty for EC2, whose keys predate the other offers
#
# Environment:
#   R2_BUCKET      bucket name (required)
#   R2_ACCOUNT_ID  Cloudflare account id, used to build the endpoint URL
#   R2_ENDPOINT    endpoint URL; overrides R2_ACCOUNT_ID
#   AWS_PROFILE    or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY: the R2 API token's credentials
#   TMPDIR         where the bundle and seed.sh's downloads go
#   RESEED         set to 1 to build a new bundle from scratch even if the bucket has one, such as after an
#                  offer's variant keys change; older bundles are left in the bucket
# Any other variable seed.sh reads (MAX_VERSIONS, HOST_SUFFIX) is passed through.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: publish-offer.sh <offer> <prefix>" >&2
  exit 2
fi
offer="$1"
prefix="$2"
if [[ ! "$offer" =~ ^[A-Za-z0-9]+$ || ! "$prefix" =~ ^[a-z0-9-]*$ ]]; then
  echo "publish: bad offer '$offer' or prefix '$prefix'" >&2
  exit 2
fi

bucket="${R2_BUCKET:?publish: R2_BUCKET is required}"
if [[ -n "${R2_ENDPOINT:-}" ]]; then
  endpoint="$R2_ENDPOINT"
elif [[ -n "${R2_ACCOUNT_ID:-}" ]]; then
  endpoint="https://$R2_ACCOUNT_ID.r2.cloudflarestorage.com"
else
  echo "publish: set R2_ACCOUNT_ID or R2_ENDPOINT" >&2
  exit 1
fi

for cmd in aws xz shasum; do
  command -v "$cmd" >/dev/null || { echo "publish: $cmd is required" >&2; exit 1; }
done

# Newer AWS CLIs send checksum headers R2 may not accept unless asked for.
export AWS_REQUEST_CHECKSUM_CALCULATION="${AWS_REQUEST_CHECKSUM_CALCULATION:-when_required}"
export AWS_RESPONSE_CHECKSUM_VALIDATION="${AWS_RESPONSE_CHECKSUM_VALIDATION:-when_required}"

r2() { aws --endpoint-url "$endpoint" --region auto "$@"; }

# exists succeeds if the key is in the bucket and fails if it isn't. Any other error (bad credentials,
# wrong bucket) exits, so it is never mistaken for an empty bucket and a full reseed.
exists() {
  local err
  if err="$(r2 s3api head-object --bucket "$bucket" --key "$1" 2>&1 >/dev/null)"; then
    return 0
  fi
  if [[ "$err" == *"404"* || "$err" == *"Not Found"* ]]; then
    return 1
  fi
  echo "publish: checking s3://$bucket/$1 failed: $err" >&2
  exit 1
}

sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/instances-publish.XXXXXX")"
trap 'rm -rf "$work"' EXIT
bundle="$work/bundle.json"

latest="${prefix}latest"
before=""
if [[ "${RESEED:-}" == 1 ]]; then
  echo "publish: RESEED is set, building a new $offer bundle from scratch" >&2
elif exists "$latest"; then
  r2 s3 cp --only-show-errors "s3://$bucket/$latest" "$work/latest"
  ts="$(tr -d '[:space:]' < "$work/latest")"
  if [[ ! "$ts" =~ ^[A-Za-z0-9._-]+$ ]]; then
    echo "publish: $latest holds '$ts', which is not a timestamp" >&2
    exit 1
  fi
  key="${prefix}bundle-$ts.json.xz"
  # Starting over here would silently replace the history with a full reseed, so stop instead.
  if ! exists "$key"; then
    echo "publish: $latest points at $key, which is not in $bucket" >&2
    exit 1
  fi
  echo "publish: downloading $key" >&2
  r2 s3 cp --only-show-errors "s3://$bucket/$key" "$bundle.xz"
  xz -d "$bundle.xz"
  before="$(sha256 "$bundle")"
  echo "publish: $key sha256 $before" >&2
else
  echo "publish: $bucket has no $latest, seeding a new $offer bundle" >&2
fi

OFFER="$offer" "$here/seed.sh" "$bundle"

after="$(sha256 "$bundle")"
if [[ "$after" == "$before" ]]; then
  echo "publish: $offer bundle unchanged, nothing to upload" >&2
  exit 0
fi

now="$(date -u +%s)"
key="${prefix}bundle-$now.json.xz"
echo "publish: bundle sha256 $after, uploading $key" >&2
xz -T0 -9 "$bundle"
r2 s3 cp --only-show-errors --content-type application/x-xz "$bundle.xz" "s3://$bucket/$key"
# Only move latest once the bundle it names is fully uploaded.
printf '%s\n' "$now" > "$work/latest"
r2 s3 cp --only-show-errors --content-type text/plain "$work/latest" "s3://$bucket/$latest"
echo "publish: $latest is now $now" >&2
