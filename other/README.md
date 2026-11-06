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
| `azure` | `AzureVM` | virtual machine sizes | Azure's Retail Prices API, with specs from Azure's pricing calculator | the first run |
| `gcp` | `GCPCompute` | Compute Engine machine types | the Cloud Billing Catalog API and the Compute Engine API (needs [credentials](#gcp-credentials)) | the first run |
| `hetzner` | `HetznerCloud` | cloud servers | the price API behind Hetzner's website, with specs from its cloud server pages | the first run |
| `hetznerauction` | `HetznerAuction` | Server Auction listings | the JSON file behind the Server Auction page | the first run |
| `ovh` | `OVHPublicCloud` | Public Cloud instance flavors | OVHcloud's public order catalog, World and US | the first run |
| `ovheco` | `OVHEco` | Kimsufi, So you Start, Rise and Advance dedicated servers | OVHcloud's public eco catalog and availability list | the first run |
| `oracle` | `OracleCompute` | VM and bare metal shapes | Oracle's price list API, with shapes from its cost estimator's data | the first run |
| `ibm` | `IBMVPC` | Virtual Server for VPC profiles | IBM Cloud's Global Catalog | the first run |

Each offer has its own reader and its own variant keys and terms (see [Offers](#offers)). Around them, the
bundle, the merge rules and the publishing are shared, so adding a provider is a reader and a line in
`offers.go`.

## Usage

```sh
cd other
go build -o other-instances .

./other-instances fetch runpod -o runpod.json                  # RunPod's current prices, as a source file
./other-instances fetch azure -o azure.json                    # Azure's current prices, as a source file
./other-instances fetch gcp -o gcp.json                        # Google Cloud's current prices (needs credentials)
./other-instances fetch hetzner -o hetzner.json                # likewise hetznerauction, ovh, ovheco, oracle and ibm
./other-instances fetch alibaba -version 2.0.426 -o ali/       # one Alibaba price version, as a directory
./other-instances versions alibaba -after 2.0.400              # Alibaba versions published since

./other-instances compress -o bundle.json <source>             # start a bundle
./other-instances append bundle.json <source>                  # add a newer source (rewrites in place unless -o)
./other-instances upgrade bundle.json                          # rewrite in the current format
go test ./...
```

`append` exits with status 3 when there is nothing to add: the source is not newer than the bundle's latest
snapshot or, for every provider but Alibaba, nothing in it changed.

The update scripts do the fetching and appending, creating the bundle if it doesn't exist:

- `other/update-runpod.sh [runpod-bundle.json]` adds RunPod's current prices. RunPod has no price history to
  download, so the bundle starts on its first run. A read that changes nothing isn't recorded, so the
  snapshots list only grows when a price, attribute or data center changes.
- `other/update-azure.sh`, `update-gcp.sh`, `update-hetzner.sh`, `update-hetznerauction.sh`, `update-ovh.sh`,
  `update-ovheco.sh`, `update-oracle.sh` and `update-ibm.sh` work the same way, each taking a bundle path, for
  the other providers that publish only their current prices.
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

### Azure (`AzureVM`)

Read from `https://prices.azure.com/api/retail/prices` (API version `2023-01-01-preview`, which adds savings
plans), filtered to `serviceName eq 'Virtual Machines'` without the Dev/Test prices: about 540,000 prices. The
API pages 1000 prices at a time, each page linking to the next, which takes over half an hour in a row. So
`fetch azure` splits the prices into parts and reads 8 at a time, about five minutes in all. A part with more
than one page is split in two by the next condition in `azureSplits` (reservation or not, Windows or not, Spot
or not, then ARM name prefixes such as `Standard_D`, `Standard_D2`). The API has no `not` or `or`, but takes
`... eq false`, so the two halves of a split never overlap or miss a price, and it rejects filters of more than
about a dozen conditions, which the splits stay within.

- **Instances** are ARM SKU names (`Standard_D2s_v3`), priced per VM per hour. Dedicated hosts (priced per
  host) and Cloud Services roles are left out.
- **Region:** the ARM region (`eastus`). The regions table gives its `regionCode`, `locationType`
  (`Azure Region`, or `Global` for the few Spot prices Azure lists as `Global`), and `name` (`US East`).
- **Variant keys:** `operatingSystem`, `Windows` for products named `... Windows`, `... Windows VM` or
  `... Win`, otherwise `Linux`; and `priority`, `Regular`, `Spot` or `Low Priority`, from the SKU name's suffix.
- **Prices** are dated by Azure's `effectiveStartDate`, or by the fetch if that is later (the API only lists
  prices in effect), or if it is no later than the key's previous row.
- **Terms:** reservations are `[1yr|3yr|5yr, Reserved Instance, All Upfront]`, `hourly` 0 and `upfront` the
  whole term's price (Azure charges the same paid monthly). Azure lists them for Linux only: a Windows
  reservation covers the same compute, with the license billed on top. Savings plans are
  `[1yr|3yr, Savings Plan, No Upfront]`, with the plan's hourly rate.
- **Attributes:** `skuName` and `productName` (from the Linux pay-as-you-go price), `productFamily`
  (`Virtual Machines`), and, from the pricing calculator (`azure.microsoft.com/api/v3/pricing/virtual-machines/calculator`),
  `cores`, `ram` (GB), `diskSize` (temporary disk, GB), `gpu`, `series` and `isVcpu`. The calculator names sizes
  differently (`linux-e4-2s-v3-standard`), so they are matched on the name's letters and digits, and `_Promo`
  sizes use their base size. The calculator doesn't list every size (about a third, mostly the newest and
  retired series, have no specs).

### Google Cloud Compute Engine (`GCPCompute`)

Read from the Cloud Billing Catalog API (`cloudbilling.googleapis.com/v1/services/6F81-5844-456A/skus`, every
Compute SKU in USD) and the Compute Engine API's `aggregated/machineTypes`, which lists every machine type in
every zone. Both need credentials; see [GCP credentials](#gcp-credentials). A fetch takes under a minute.

Most machine types have no SKU of their own: Compute Engine bills each family's vCPUs and memory, and GPUs,
separately. So a machine type's price is worked out from its shape:
`guestCpus × the family's vCPU rate + memoryMb / 1024 × its rate per GiB + each GPU × the GPU's rate`.
For example, n2-standard-2 in us-central1 is 2 × $0.031611 + 8 × $0.004237 = $0.097118.

- **Instances** are machine type names (`n2-standard-2`).
- **Families:** a machine type's vCPU and memory SKUs are found by its family's SKU name: usually its name up to
  the first `-` (`n2` matches `N2 Instance Core`, `C4A Arm Instance Core` is `c4a`), and for the rest
  `gcpFamilyStems` (C2 is `Compute optimized`, M1 and M2 `Memory-optimized`, a3-megagpu `A3Plus`, and so on).
  GPUs come from the machine type's `accelerators`, matched by `gcpGPUStems`. A4 is billed by GPU slice, which
  includes its share of the vCPUs and memory. Shared-core E2 types are billed for a share of a vCPU (e2-micro
  0.25, e2-small 0.5, e2-medium 1); f1-micro and g1-small have their own SKUs.
- **Left out:** sole tenancy, custom machine types, reservations, Dynamic Workload Scheduler and premium SKUs;
  bundled Local SSD (`-lssd` types and others list `bundledLocalSsds.partitionCount` but are priced without
  it); OS licenses (prices are for free images); sustained use discounts. TPU machine types, X4 and X5 bare
  metal, and A4X have no per-vCPU SKUs, so they are left out, and `append` prints how many there were. A
  machine type is priced in a region only if every part of its price has a SKU there.
- **Region:** the region (`us-central1`), with `regionCode` and `locationType` (`Google Cloud Region`). Each
  machine type's `zones` attribute lists the zones in the region that have it.
- **Variant key:** `provisioningModel`, `Standard` or `Spot` (`Spot Preemptible ...` SKUs).
- **Terms:** resource-based committed use discounts, `[1yr|3yr, Committed Use, No Upfront]`, with the
  commitment's hourly rate for the machine type's vCPUs, memory and GPUs. Shared-core types have none.
- **Attributes:** the machine type's fields (`guestCpus`, `memoryMb`, `description`, `isSharedCpu`,
  `architecture`, `maximumPersistentDisks`, `bundledLocalSsds.partitionCount`, ...), `acceleratorType` and
  `acceleratorCount`, `deprecated` (its state), and `productFamily` (`Compute Engine`).
- **Gaps in Google's data:** a few regions list two SKUs for one family (`Memory-optimized ... in Virginia` and
  `... in Northern Virginia`). The lower price is kept, and `append` prints how many there were.

### Hetzner cloud servers (`HetznerCloud`)

Hetzner's Cloud API needs a token and answers in the account's currency, so prices come from the public API behind
its website, `website-price-api.hetzner.com/api/v1/products/<id>`, which answers in USD and EUR without a key. That
API only knows servers by product id (`CLOUD_132`), so `fetch hetzner` reads each server's name, specs and product id
from the rows of `hetzner.com/cloud/cost-optimized/`, `regular-performance/` and `general-purpose/`. **This is the one
source here that is parsed from a web page.** A row it can't read stops the fetch rather than dropping the server, so a
redesign fails the publish job and leaves the published bundle alone. Servers no longer on those pages are recorded as
deleted, even if existing ones keep running.

- **Instances** are the Cloud API's names (`cx23`), priced per server per hour.
- **Region:** the Cloud API's location names (`fsn1`, `ash`), with `name`, `country` and `locationType`
  (`Hetzner Location`).
- **Variant key:** `ipv4`, `IPv6 only` or `Primary IPv4`. A server's price leaves out its primary IPv4, which is optional
  and billed apart ($0.001 an hour, $0.60 a month). `Primary IPv4` adds it, and is what the website shows.
- **Terms:** the monthly cap, `[1mo, Monthly Cap, Postpaid]`, `hourly` 0 and `upfront` the cap. Hetzner bills by the hour
  up to the cap at the end of the month, so nothing is paid up front; the field holds it because it is the term's total.
- **Attributes:** `name`, `category`, `cores`, `cpu`, `architecture` (`arm64` for Ampere), `memoryInGb`, `diskInGb`,
  `diskType` and `productFamily`. Availability isn't kept: it changes too often to be history.

### Hetzner Server Auction (`HetznerAuction`)

Read from `hetzner.com/_resources/app/data/app/live_data_sb.json`, the file behind the Server Auction page. Each listing is
one physical server, so instances are listing ids (`3107671`), which are only meaningful while the listing is up: a
server's history runs from the read that first sees it to the read after it is sold, when it is recorded as deleted.
Auction prices fall on a timer, and the hourly read records each fall, not when within the hour it happened.

- **Region:** the data center's site (`fsn1`), with the full name (`FSN1-DC4`) as the `datacenter` attribute.
- **Variant key and terms:** as for cloud servers, using the listing's `IPPrices` for the primary IPv4.
- **Attributes:** the listing's `Hardware` and `Details` (`CPU.Name`, `RAM.Size`, `Storage.Disks`, `Specials`, ...),
  `fixedPrice` and `setupFee`. The countdown to the next price drop is left out.

### OVHcloud Public Cloud (`OVHPublicCloud`)

Read from `/order/catalog/public/cloud` on OVHcloud's World (`ca.api.ovh.com`, subsidiary `WS`) and US
(`api.us.ovhcloud.com`) APIs, the two that sell in USD. Customers of OVHcloud's European subsidiaries are charged in EUR,
at list prices that aren't these converted.

- **Instances** are flavor names (`b3-8`), priced per hour. The Windows plan `win-b2-7` is `b2-7` with the `Windows`
  variant of `operatingSystem`.
- **Region:** the catalog prices by zone, and the list of regions in each zone needs a login, so the regions table has
  pricing zones, not regions: `world` (standard regions), `3az`, `lz-eu`, `lz-eurozone` and `lz-af` (Local Zones), `us` and
  `us-east-lz-dal-a`.
- **Terms:** monthly plans, `[1mo, Monthly, Postpaid]` for `.monthly.postpaid` and `[1mo, Monthly, All Upfront]` for the US
  catalog's `.monthly`, with `hourly` 0 and `upfront` the month's price. Newer flavors such as `b3-8` have none.
- **Attributes:** the flavor's `blobs.technical`, flattened (`cpu.cores`, `memory.size`, `gpu.model`, `storage.disks`, ...),
  `brick`, `brickSubtype` and `productFamily`.

### OVHcloud eco dedicated servers (`OVHEco`)

OVHcloud has no server auction. Its eco ranges (Kimsufi, So you Start, Rise and Advance, which the `range` attribute names)
are the closest it has, at fixed prices. Read from `/order/catalog/public/eco` in USD, with the configurations from
`/dedicated/server/datacenter/availabilities`.

- **Instances** are server products (`24sys01`). **Unlike every other offer, these have no on-demand prices:** the servers can
  only be rented by the month, so an hourly price would be a number OVHcloud doesn't charge. The rent is the term below, and
  `billing` is `Monthly`. To compare with other clouds, divide the rent by the hours in a month.
- **Region:** the data center (`gra`, `rbx`, `sgp`). A server sold in several sets of data centers has a plan per set, whose
  code is the `planCode` attribute.
- **Variant keys:** `memory`, `storage` and `systemStorage`, as the availability list names them. A configuration's price is
  the plan's plus its options' and any other mandatory addon's. Options the list names but the catalog has no price for
  (a few dozen, usually older memory speeds) are left out with a warning.
- **Terms:** the month's rent, `[1mo, Rental, All Upfront]`, and prepaying, `[1yr|2yr, Prepaid, All Upfront]`, all with `hourly` 0
  and `upfront` the whole term's price. Prepaying also waives the setup fee.
- **Attributes:** the product's `blobs.technical.server` (`range`, `cpu.model`, `cpu.cores`, `frame.size`, ...), `invoiceName`,
  `planCode`, `setupFee` (of the plan, in USD, once) and `productFamily`. Stock levels are left out.

### Oracle Cloud compute (`OracleCompute`)

Read from Oracle's price list API (`apexapps.oracle.com/pls/apex/cetools/api/v1/products/?currencyCode=USD`), with what each
shape is billed for from its cost estimator's `productpresets.json`. Only `VM.` and `BM.` shapes are kept: those data also
list shapes for Container Instances, dedicated VM hosts, Kubernetes pods and databases.

- **Instances:** fixed shapes by name (`BM.GPU.H100.8`). Flexible shapes have no one price, so each is priced at powers of
  two OCPUs and its maximum, at 4, 8 and 16 GB per OCPU within its maximum memory, and named `<shape>-<ocpus>-<GB>`
  (`VM.Standard.E5.Flex-2-16`), **which are not names Oracle uses**. The `shape` attribute is Oracle's name, and `ocpuPrice`
  and `memoryGbPrice` give any other size. Flexible Dense I/O shapes come in steps of 8 OCPUs with memory and NVMe in
  proportion, so they are priced at those.
- **Prices:** every part's quantity times its rate: `ocpus × OCPU rate + GB × memory rate + TB × NVMe rate`, or for GPU shapes
  `GPUs × GPU rate`. Older shapes include memory in the OCPU rate. A part with a free tier (A1) has its paid rate.
- **Region:** Oracle charges the same in every commercial region, so every row is in `global`.
- **Variant key:** `operatingSystem`, `Linux`, or `Windows` for shapes that support it, which adds the Windows part's rate per OCPU.
- **Left out:** preemptible (half price, but not listed as a price), burstable and capacity reservations, and other paid images.
- **Attributes:** `shape`, `shapeType`, `processor`, `ocpus`, `memoryInGb`, `description`, `localDisk`, `gpuCount`, `gpuMemoryInGb`,
  `nvmeInTb`, `allowPreemptible` and `allowBurstable`, and for flexible shapes `flexible`, `maxOcpus` and `maxMemoryInGb`.

### IBM Cloud Virtual Server for VPC (`IBMVPC`)

Read from the Global Catalog (`globalcatalog.cloud.ibm.com/api/v1`): every `instance.profile`, then for each pricing plan they
use, `/<plan>/pricing/deployment`, which has its prices per region. That is several hundred plans, read 8 at a time, which
takes seconds. Only deployments with the origin `pricing_catalog` that are a whole region are kept, as shared plans also list
single zones and older per-profile deployments with other prices. Plans the catalog won't show are skipped.

- **Instances** are profile names (`bx3d-2x10`), priced per hour. Classic infrastructure and bare metal aren't included.
- **Prices** are dated by the catalog's `effective_from`, or by the fetch if that is later. Newer profiles have their own
  price (`part-is.instance-hours-<profile>`). About 90 older ones (`bx2`, `cx2`, `mx2`, `gx2`, ...) share a plan **whose
  per-vCPU and per-GB rates don't add up to what IBM charges**, a third of the price for GPU profiles. So they use the
  per-profile rate for an hourly reservation. **That is an approximation.** It can be checked on the 67 profiles that have
  both prices public: as of November 2026 it matches for 54, and for the other 13 (some `mx3d`, `gx3` and `bx3d` sizes) it is
  between 9% under and 44% over. Profiles with neither (`ux2d-96x2800`, `vx2d-44x616`, whose plans are private)
  are left out, as are the oldest profiles, which have no plan.
- **Region:** the multizone region (`us-south`), limited to the regions the profile lists, with `name` and `locationType`.
- **Variant key:** `capacityType`, `On-Demand`, `Spot`, or `Burstable 50%`, `25%` or `10%`.
- **Terms:** `[1yr|3yr, Reserved, No Upfront]`, with the reservation's hourly rate, where its plan is public.
- **Left out:** operating system and software licenses (prices are for free images) and dedicated hosts.
- **Attributes:** the profile's `default_config`, flattened (`cpu`, `ram`, `vcpu_manufacturer`, `gpu_model`, `disks`, ...), with
  `displayName`, `family`, `generation` and `productFamily`.

### GCP credentials

`fetch gcp` needs an OAuth access token and a project to list machine types in. It looks, in order, for:

- `GCP_ACCESS_TOKEN` and `GCP_PROJECT`, a token from elsewhere (`gcloud auth print-access-token`);
- `GCP_SERVICE_ACCOUNT_KEY`, the contents of a service account key;
- `GOOGLE_APPLICATION_CREDENTIALS`, the path of a service account key or authorized user file;
- gcloud's application default credentials (`gcloud auth application-default login`).

`GCP_PROJECT` overrides the key's project. The token needs `cloud-billing.readonly` and `compute.readonly`
scopes; any identity can read the catalog, and listing machine types needs `compute.machineTypes.list` in the
project. The module keeps to the standard library, so `gcpauth.go` signs service account tokens and refreshes
user tokens itself.

In CI, nothing secret is stored. The `gcp` job gets a one-hour token through workload identity federation
(`google-github-actions/auth`) as the service account
`instances-bundler-pricing@consummate-atom-503516-h4.iam.gserviceaccount.com`, which has one custom role,
`machineTypeReader` (`compute.machineTypes.list` and `.get`). The pool `instances-bundler-github` only accepts
tokens from this repository's `publish-script.yml` on `main`, run by a schedule or by hand. The repository
variables `GCP_PROJECT`, `GCP_SERVICE_ACCOUNT` and `GCP_WORKLOAD_IDENTITY_PROVIDER` name them, and only the
`gcp` job in `publish.yml` grants `id-token: write`.

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

The top-level `publish-<platform>.sh` scripts keep these bundles up to date in R2, in the same
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

`.github/workflows/publish.yml` runs RunPod every 30 minutes, Azure, GCP, Hetzner, OVHcloud and Oracle hourly, and Alibaba and IBM every 6 hours.

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
azure.go        Azure fetcher (parallel, split by filters) and reader
gcp.go          GCP fetcher and reader, which prices machine types from their shape
gcpauth.go      GCP access tokens from service account keys and user credentials
hetzner.go      Hetzner cloud server and Server Auction fetchers and readers
ovh.go          OVHcloud Public Cloud and eco fetchers and readers
oracle.go       Oracle fetcher and reader, which prices shapes from their parts
ibm.go          IBM fetcher and reader
bundle_test.go  readers, history rules, fetchers against fake servers
cloud_test.go   the same for Azure and GCP
providers_test.go  the same for Hetzner, OVHcloud, Oracle and IBM
```
