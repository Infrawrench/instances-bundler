# other-instances

Keeps the history of instance attributes and prices for clouds other than AWS, one small JSON bundle per
offer. The bundles use instances-bundler's format (the [AWS tool](../aws) in this repository), so the
bulk-instances SDKs download and decode them by platform name, unchanged, just as they do the AWS ones:

```ts
const runpod = await downloadAwsPricingData("runpod");
runpod.get("NVIDIA H100 80GB HBM3")?.pricing.onDemand;
```

| Platform | Offer | What | Source | History from |
| --- | --- | --- | --- | --- |
| `runpod` | `RunpodPods` | GPU types and CPU pod sizes | RunPod's public GraphQL API | the first run |
| `alibaba` | `AlibabaECS` | ECS instance types | the versioned price package behind Alibaba Cloud's ECS pricing page | May 2018 (version 2.0.0) |

Each offer has its own reader and its own variant keys and terms (see [Offers](#offers)). Around them, the
bundle, the merge rules and the publishing are shared, so adding a provider is a reader and a line in
`offers.go`.

## Usage

```sh
cd other
go build -o other-instances .

./other-instances fetch runpod -o runpod.json                  # RunPod's current prices, as a source file
./other-instances fetch alibaba -version 2.0.426 -o ali/       # one Alibaba price version, as a directory
./other-instances versions alibaba -after 2.0.400              # Alibaba versions published since

./other-instances compress -o bundle.json <source>             # start a bundle
./other-instances append bundle.json <source>                  # add a newer source (rewrites in place unless -o)
./other-instances upgrade bundle.json                          # rewrite in the current format
go test ./...
```

`append` exits with status 3 when there is nothing to add: the source is not newer than the bundle's latest
snapshot or, for RunPod, nothing in it changed.

The update scripts do the fetching and appending, creating the bundle if it doesn't exist:

- `other/update-runpod.sh [runpod-bundle.json]` adds RunPod's current prices. RunPod has no price history to
  download, so the bundle starts on its first run. A read that changes nothing isn't recorded, so the
  snapshots list only grows when a price, attribute or data center changes.
- `other/update-alibaba.sh [alibaba-bundle.json]` adds every Alibaba version published since the bundle's
  `source_version`, oldest first. Without a bundle it seeds from version 2.0.0: 408 versions, about 10 GB of
  downloads, one version on disk at a time. `MAX_VERSIONS=n` stops after `n`. Re-running resumes.

## Offers

### RunPod (`RunpodPods`)

Read from `https://api.runpod.io/graphql`, which answers price queries without an API key: `gpuTypes`,
`cpuFlavors`, `dataCenters`, and `cpuFlavors.specifics` for each CPU pod size. `fetch runpod` saves the
answers as they came, with the time they were read (`fetchedAt`), which dates the snapshot.

- **Instances** are GPU types by RunPod id (`NVIDIA H100 80GB HBM3`), priced per GPU per hour, and CPU pods
  by pod id (`cpu3c-2-4`: flavor, vCPUs, GB of memory), priced per pod per hour. CPU pod sizes are each
  flavor's powers of two from `minVcpu` to `maxVcpu`, with memory `vcpu × ramMultiplier`, the ids RunPod's
  deploy API takes. `productFamily` (`GPU` or `CPU`) and `priceUnit` (`GPU-Hour` or `Hour`) tell them apart.
- **Region:** prices are the same in every data center, so every row is in `global`. The regions table also
  lists each data center (`location`, `storageSupport`, `listed`), and a GPU's `dataCenters` attribute lists
  the ones that stock it.
- **Variant key:** `cloudType`, `Secure Cloud` or `Community Cloud`. A price is only recorded for a cloud
  whose flag (`secureCloud`, `communityCloud`) is set; the API keeps a price for clouds that don't sell the GPU.
- **Terms:** savings plans (`threeMonthPrice` and so on) are stored as reserved rows with the term
  `[3mo, Savings Plan, All Upfront]` in Secure Cloud. RunPod quotes them as an effective hourly rate, so that is
  what `hourly` holds, and `upfront` is left out.
- **Attributes:** every other GPU field (`displayName`, `manufacturer`, `memoryInGb`,
  `maxGpuCountSecureCloud`, ...), and for CPU pods the flavor's fields plus `flavor`, `vcpu` and `memoryInGb`.
- **Left out:** stock levels, which change too often to be history; spot prices, which the public API
  returns as null for every GPU; serverless prices.

### Alibaba Cloud ECS (`AlibabaECS`)

Read from `https://g.alicdn.com/aliyun/ecs-price-info-intl/2.0.<n>/`, the international site's price
package. Every version stays published, numbered 2.0.0 to 2.0.426 so far with a few numbers skipped, so
`versions alibaba` probes upward until 10 numbers in a row are missing. Each version has:

| File | Has | From |
| --- | --- | --- |
| `price/download/instancePrice.json` | pay-as-you-go and subscription prices, and the `publicationDate` that dates the snapshot | 2.0.0 |
| `price/js_price/filter_price/reservedInstancePrice.js` | reserved instance prices | 2.0.20 |
| `price/js_price/filter_price/instanceTypeDefinition.js` | `DescribeInstanceTypes` for every type, used as attributes | 2.0.210 |

- **Region:** the region id (`cn-hangzhou`). The regions table gives its `regionCode`, `locationType`
  (`Alibaba Cloud Region`), and an English `name` for the regions listed in `alibaba.go`.
- **Variant keys:** `operatingSystem` (`Linux`, `Windows`) and `networkType` (`VPC`, or `Classic`, which
  versions up to 2023 also priced). Every version so far prices only I/O optimized instances; anything else
  would be counted and left out with a warning.
- **On-demand:** the pay-as-you-go hourly `price`, the price charged. `originalPrice`, the list price before
  promotions, is left out.
- **Terms:** subscriptions (1 month, 1 to 5 years) are paid in full when bought, so they are stored like AWS
  All Upfront reservations: term `[1yr, Subscription, All Upfront]`, `hourly` 0, `upfront` the price.
  Reserved instances keep their own terms, such as `[3yr, Reserved Instance, Partial Upfront]`, with
  `reservedHourPrice` as `hourly` and `prepayPrice` as `upfront`.
- **Attributes:** the instance type's definition, flattened: nested objects as dotted keys
  (`CpuOptions.ThreadsPerCore`), lists joined with commas, numbers as Alibaba wrote them (`MemorySize` `8.0`),
  empty values left out. Types with prices before 2.0.210 have no attributes until then.
- **Gaps in Alibaba's data:** many versions have a few hundred reserved prices whose `prepayPrice` is a word
  instead of a number. `PriceNotFound` means not offered. Any other word is an error Alibaba hit while it
  built the file (`Throttling`, `InvalidVersion`), so the price is unknown: the bundle keeps the price it had
  rather than recording the reservation as withdrawn and offered again a version later, and `append` prints
  how many there were.

## Format

The bundle is instances-bundler's format (see the [top-level README](../README.md#format) for the full description), with the offer and its
variant keys always written, and one extra field, `source_version`, that the SDKs ignore:

```jsonc
{
  "offer": "AlibabaECS",
  "source_version": "2.0.426",              // the provider's name for the latest snapshot, if it has one
  "compression_table": ["Linux", "VPC", ...],   // every string, most used first; everything else is an index
  "snapshots": [1526550243, ...],          // when each snapshot was published (or fetched), oldest first
  "regions": [[<location>, [[<key>, <value>], ...]]],
  "variant_keys": ["operatingSystem", "networkType"],
  "variants": [[<value>, ...]],
  "reserved_terms": [[<lease>, <offeringClass>, <purchaseOption>]],
  "instances": {
    "ecs.g6.large": [
      [[<key>, <value>]],                                  // 0 - base attributes
      [[<ts>, <key>, <value|null>, <region?>]],            // 1 - attribute changes
      0.1068,                                              // 2 - base on-demand price (the most common one)
      [[<ts>, <region>, <variant>, <price?|null>]],        // 3 - on-demand prices; omitted = base, null = gone
      [[<ts>, <region>, <variant>, <term>, <hourly|null>, <upfront?>]],  // 4 - committed prices
      1526550243,                                          // 5 - created_at
      null                                                 // 6 - deleted_at
    ]
  }
}
```

A price at time `t` is the row with the newest `ts <= t` for its key, and attributes are the base attributes
with the changes up to `t` applied. Rows are only added when something changes.

## Publishing

The top-level `publish-runpod.sh` and `publish-alibaba.sh` keep these bundles up to date in R2, in the same
bucket as the AWS ones, through the shared `publish-offer.sh` (see the [top-level README](../README.md#publishing)):
it downloads the bundle `<platform>-latest` points at, runs `other/update-<platform>.sh` on it, and if the bundle
changed, uploads `<platform>-bundle-<now>.json.xz` and moves `<platform>-latest`.

```sh
R2_BUCKET=my-bucket R2_ACCOUNT_ID=abc123 \
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... ./publish-alibaba.sh
```

If the bucket has no `<platform>-latest`, the update script builds the bundle from scratch, which for Alibaba
downloads every version. `SEED_BUNDLE=path` starts from a bundle built locally instead, and `RESEED=1`
rebuilds a published bundle (needed if an offer's variant keys change).

`.github/workflows/publish.yml` runs RunPod every 30 minutes and Alibaba every 6 hours.

## Adding a provider

1. Write a reader that turns a fetched source into a `snapshot` (see `runpod.go`, `alibaba.go`), and a
   `fetch` case for it.
2. Add its offer to `offers.go`: a platform name (lowercase letters and digits, which is what the SDKs
   accept), its variant keys, and whether it is `Polled` (a live API with no publication dates).
3. Add an `other/update-<platform>.sh`, a top-level `publish-<platform>.sh` that calls `publish-offer.sh`
   with it, and a cron and job in `publish.yml` with `go-module: other`.

## Layout

```
main.go         command-line interface
offers.go       offers, snapshot types
bundle.go       bundle reading and writing (the AWS tool's format)
merge.go        appending a snapshot: rows only where something changed
state.go        reading prices and attributes back out (copied from ../aws)
attrs.go        flattening provider JSON into attributes
runpod.go       RunPod fetcher and reader
alibaba.go      Alibaba fetcher, version prober and reader
bundle_test.go  readers, history rules, fetchers against fake servers
```
