#!/usr/bin/env bash
# Builds a spot bundle from the last 90 days of EC2 spot price history, then adds the current spot price
# feed on top, so publish-spot.sh can carry on from it.
#
# The history comes from EC2's DescribeSpotPriceHistory, which needs AWS credentials and keeps 90 days. It is
# per availability zone, while the feed is per region, so zone prices are combined into a region price
# (AGGREGATE, the lowest by default). instances-bundler prints how well each way of combining them matches
# the feed; check that before publishing.
#
#   AWS_PROFILE=spot-history ./seed-spot.sh [spot-bundle.json]
#   SEED_BUNDLE=spot-bundle.json R2_BUCKET=... ./publish-spot.sh   # then publish it
#
# History pages are kept in HISTORY_DIR, one directory per region, so an interrupted run can be resumed by
# running it again. Running it again after a region has finished tops that region up with the history
# since it was last fetched, and the bundle is rebuilt from all of it with the current feed. That way a
# bundle that sat unpublished can be brought up to date without losing the hours in between. Delete
# HISTORY_DIR to fetch everything again.
#
# Environment:
#   AWS_PROFILE    or other AWS credentials allowed ec2:DescribeRegions and ec2:DescribeSpotPriceHistory.
#                  Not the R2 token publish-offer.sh uses.
#   HISTORY_DIR    where history pages are saved (default ./spot-history)
#   REGIONS        space-separated regions to fetch (default: every region enabled in the account)
#   PARALLEL       regions fetched at once (default 6)
#   DAYS           days of history (default 90, the most EC2 keeps)
#   AGGREGATE      how zone prices become a region price: min, max, mean or median (default min)
#   INTERVAL       at most one snapshot per interval (default 1h); rows keep their own times
set -euo pipefail

bundle="${1:-spot-bundle.json}"
history="${HISTORY_DIR:-spot-history}"
parallel="${PARALLEL:-6}"
days="${DAYS:-90}"
feed="https://website.spot.ec2.aws.a2z.com/spot.json"

for cmd in aws curl jq gzip go; do
  command -v "$cmd" >/dev/null || { echo "seed-spot: $cmd is required" >&2; exit 1; }
done

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/instances-bundler"
(cd "$here" && go build -o "$bin" .)

# Retry throttled calls, which a 90-day fetch makes a lot of.
export AWS_RETRY_MODE="${AWS_RETRY_MODE:-adaptive}"
export AWS_MAX_ATTEMPTS="${AWS_MAX_ATTEMPTS:-10}"
export AWS_PAGER=""

identity="$(aws sts get-caller-identity --query Arn --output text)"
echo "seed-spot: using $identity" >&2

mkdir -p "$history"
# The start is fixed on the first run, so resumed regions all cover the same span. It is an hour inside
# the limit, since EC2 rejects start times more than $days days ago.
if [[ ! -f "$history/start" ]]; then
  jq -rn --argjson days "$days" 'now - $days * 86400 + 3600 | floor | todate' > "$history/start"
fi
start="$(cat "$history/start")"

if [[ -n "${REGIONS:-}" ]]; then
  read -r -a regions <<< "$REGIONS"
else
  read -r -a regions <<< "$(aws ec2 describe-regions --region us-east-1 --query 'Regions[].RegionName' --output text)"
fi
echo "seed-spot: fetching history since $start for ${#regions[@]} region(s) into $history" >&2

# fetch_region saves one region's history as gzipped pages of up to 10,000 records, then writes when the
# fetch began to its done file. If the region was fetched before, it fetches only what came after that.
# The overlap repeats a few records, which is harmless.
fetch_region() {
  local region="$1" dir="$history/$1" from="$start" token="" n=0 records=0 page began segment
  began="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if [[ -f "$dir/done" ]]; then
    from="$(cat "$dir/done")"
    echo "seed-spot: $region: topping up from $from" >&2
  else
    rm -rf "$dir"
    mkdir -p "$dir"
  fi
  # Pages are named by the time they start from, so a retried top-up replaces its own pages.
  segment="$(tr -d ':-' <<< "$from")"
  rm -f "$dir/$segment"-*.json.gz
  while :; do
    n=$((n + 1))
    page="$dir/$segment-$(printf '%06d' "$n").json"
    local args=(ec2 describe-spot-price-history --region "$region" --start-time "$from"
      --product-descriptions "Linux/UNIX" "Linux/UNIX (Amazon VPC)" "Windows" "Windows (Amazon VPC)"
      --page-size 1000 --max-items 10000 --output json)
    [[ -n "$token" ]] && args+=(--starting-token "$token")
    if ! aws "${args[@]}" > "$page"; then
      echo "seed-spot: $region failed on page $n; run again to retry it" >&2
      return 1
    fi
    token="$(jq -r '.NextToken // empty' "$page")"
    records=$((records + $(jq '.SpotPriceHistory | length' "$page")))
    gzip -f "$page"
    (( n % 10 == 0 )) && echo "seed-spot: $region: $records records so far" >&2
    [[ -z "$token" ]] && break
  done
  printf '%s\n' "$began" > "$dir/done"
  echo "seed-spot: $region done, $records records" >&2
}
export -f fetch_region
export history start

status=0
printf '%s\n' "${regions[@]}" | xargs -P "$parallel" -I{} bash -c 'set -euo pipefail; fetch_region "$1"' _ {} || status=$?
if [[ "$status" -ne 0 ]]; then
  echo "seed-spot: some regions failed; run again to fetch them" >&2
  exit 1
fi

# The feed goes last, so the history stops when it was published. -R dates it by its Last-Modified header.
curl -fsSL -R --retry 5 --retry-all-errors --retry-delay 10 -o "$history/spot.json" "$feed"

rm -f "$history/bundle.json"
"$bin" spot-history -start "$start" -aggregate "${AGGREGATE:-min}" -interval "${INTERVAL:-1h}" \
  -o "$history/bundle.json" "$history/spot.json" "$history"
mv "$history/bundle.json" "$bundle"
echo "seed-spot: done; publish it with SEED_BUNDLE=$bundle ./publish-spot.sh" >&2
