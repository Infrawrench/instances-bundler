# instances-bundler

Compresses AWS price list files (`index.json` from the AWS Price List bulk API) into one small JSON file per offer. The file keeps the history of every instance's attributes, on-demand prices and reserved prices across many files.

A 1.5 GB AWS EC2 file becomes about 30.5 MB of minified JSON (5.4 MB gzipped). Each later file adds only the rows that changed.

Supported offers, all priced per instance type:

| Platform | Offer code | Fields that pick a price (`variant_keys`) |
| --- | --- | --- |
| ec2 | `AmazonEC2` | operatingSystem, tenancy, licenseModel, preInstalledSw, capacitystatus |
| rds | `AmazonRDS` | databaseEngine, databaseEdition, licenseModel, deploymentOption, deploymentModel, engineMediaType, engineCode, usagetype |
| elasticache | `AmazonElastiCache` | cacheEngine, usagetype |
| opensearch | `AmazonES` | usagetype |
| redshift | `AmazonRedshift` | usagetype |
| docdb | `AmazonDocDB` | usagetype |
| neptune | `AmazonNeptune` | m-ncu, mncu, usagetype |
| memorydb | `AmazonMemoryDB` | engine, usagetype |
| dax | `AmazonDAX` | usagetype |
| sagemaker | `AmazonSageMaker` | component, platoinstancetype, usagetype |
| emr | `ElasticMapReduce` | softwareType, usagetype |
| gamelift | `AmazonGameLift` | operatingSystem, isSpotInstance, operation, usagetype |

The tool reads the offer from the file's `offerCode`, so there is no flag for it. Offers are configured in `offers.go`. Each offer's keys were picked so that no two products in one region share a price key in any published version. The exceptions are a few AWS mislabels in old SageMaker files, where one price is kept.

## Usage

```sh
go build -o instances-bundler .

# Start a bundle from one AWS pricing file
./instances-bundler compress -o bundle.json index.json

# Add a newer pricing file (rewrites bundle.json in place unless -o is given)
./instances-bundler append bundle.json index-next.json

# Read the pricing file from stdin
curl -s https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/index.json \
  | ./instances-bundler append bundle.json -

# Rewrite a bundle made by an older version in the current format
./instances-bundler upgrade bundle.json
```

To build a bundle from every published version of an offer's price list, run `./seed.sh [bundle.json]`, with `OFFER=AmazonRDS` (or another offer code) for anything but EC2. The first version creates the bundle and the rest are appended. Re-running it skips versions the bundle already has, so it can be stopped and resumed.

A version's ID is usually its `publicationDate`, but not always: `20190828220000` was published at `2019-08-28T20:49:02Z`. So for versions near the bundle's latest, the script reads the real date from the first 4 KB of the file before downloading the rest.

A full EC2 seed is 123 versions, about 400 GB of downloads; recent files are about 9 GB each. The other offers are much smaller. The largest is RDS: 119 versions, with recent files about 600 MB. Only one file is on disk at a time, in `$TMPDIR`. Set `MAX_VERSIONS=n` to add only the next `n` versions.

Append files oldest first, and only files for the bundle's offer. `append` rejects a file whose `publicationDate` is not newer than the bundle's latest, exiting with status 3 so scripts can tell that apart from a real failure.

`append` and `upgrade` both convert older bundles automatically, for example moving `regionCode` and `locationType` off instances into the `regions` table.

`-host-suffix` (default `.host`) is added to EC2 Dedicated Host names. AWS names hosts by family (`m5`), and `.metal` would collide with real bare-metal instances such as `m5.metal`. Use the same suffix for every append to a bundle.

Products without an `instanceType` (data transfer, EBS, IPs, load balancers, storage, and so on) are dropped, as are a few families that have one but aren't priced per instance hour (Redshift concurrency scaling and managed storage, DocumentDB CPU credits).

## Publishing

`publish-<platform>.sh` keeps one platform's bundle up to date in a Cloudflare R2 bucket. Each script calls `publish-offer.sh <offer> <prefix>`. All platforms share one bucket and are kept apart by a key prefix:

| Platform | Keys |
| --- | --- |
| ec2 | `latest`, `bundle-<timestamp>.json.xz` |
| rds, elasticache, … | `<platform>-latest`, `<platform>-bundle-<timestamp>.json.xz` (e.g. `rds-latest`) |

EC2 keeps its unprefixed keys because they were in use before the other platforms were added. `<prefix>latest` holds a timestamp. The script downloads the bundle it points at, runs `seed.sh` on it, and if anything was added uploads it as a new `<prefix>bundle-<now>.json.xz` before moving `<prefix>latest`.

```sh
R2_BUCKET=my-bucket R2_ACCOUNT_ID=abc123 \
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... ./publish-rds.sh
```

If the bucket has no `<prefix>latest`, the script seeds a new bundle from scratch. For EC2 that is a 400 GB seed, so seed EC2 from a machine that already has a bundle rather than from CI. The other platforms can seed from scratch in CI on their first run. To rebuild an existing bundle from scratch, for example after changing an offer's `variant_keys` (which makes the old bundle unreadable), run its script with `RESEED=1`. That publishes a new bundle and moves `<prefix>latest` to it, leaving the old bundles in the bucket. `publish-script.yml` has a matching `reseed` input.

`.github/workflows/publish.yml` has one cron and one job per platform: EC2 every 5 minutes, and the others every 15 minutes at staggered offsets. All run on `ubuntu-24.04-arm`. Each job runs the steps in `publish-script.yml`, and only one run per platform happens at a time. The jobs read the repository variables `R2_BUCKET` and `R2_ACCOUNT_ID`, and the secrets `R2_ACCESS_KEY_ID` and `R2_SECRET_ACCESS_KEY` (an R2 API token with read and write access to the bucket).

To add a platform, add its offer to `offers.go`, add a `publish-<platform>.sh`, and add a cron and a job to `publish.yml`.

## Format

All strings live in `compression_table`, most used first, and everything else refers to them by index. Timestamps are Unix seconds.

```jsonc
{
  "offer": "AmazonRDS",                    // left out of EC2 bundles; a bundle without it is EC2
  "compression_table": ["US West (Oregon)", "US East (N. Virginia)", ...],
  "snapshots": [1600976855, ...],          // publicationDate of every file appended, oldest first

  "regions": [                             // attributes of the location itself, newest value
    [<location>, [[<key>, <value>], ...]]  // e.g. "US East (N. Virginia)": regionCode us-east-1, locationType AWS Region
  ],

  "variant_keys": ["databaseEngine", ...], // names of the variant fields; left out of EC2 bundles, whose keys are
                                           // operatingSystem, tenancy, licenseModel, preInstalledSw, capacitystatus
  "variants": [                            // the product fields that pick a price, one per variant key
    [<value>, ...]
  ],
  "reserved_terms": [
    [<lease>, <offeringClass>, <purchaseOption>]
  ],

  "instances": {
    "m5.large": [
      [ // 0 - base attributes
        [<key>, <value>]
      ],
      [ // 1 - attribute changes
        [<ts>, <key>, <value, null = removed>, <region, omitted if global>]
      ],
      0.096, // 2 - base on-demand price
      [ // 3 - on-demand prices
        [<ts>, <region>, <variant>, <price, omitted = base price, null = no longer offered>]
      ],
      [ // 4 - reserved prices
        [<ts>, <region>, <variant>, <reserved_term>, <hourly, null = no longer offered>, <upfront, omitted if 0>]
      ],
      1600976855, // 5 - created_at: publicationDate of the first file that listed it
      null        // 6 - deleted_at: publicationDate of the first file that stopped listing it
    ]
  }
}
```

### Reading prices and attributes at a time `t`

- **Prices:** for each `(region, variant)` on-demand key, or `(region, variant, reserved_term)` reserved key, take the row with the newest `ts <= t`. Later rows win ties. A `null` price means the key is not offered.
- **Attributes in a region:** start from the base attributes and apply global changes in `ts` order. Then apply that region's changes on top. A regional change keeps applying even after a newer global change to the same key.
- **Timestamps:** price rows are dated by the AWS `effectiveDate`. Attribute changes and removals are dated by the file's `publicationDate`.
- **Deleted instances:** when an instance is deleted, removal rows are written for all of its prices, so the rules above still hold without checking `deleted_at`. If a later file lists the instance again, `deleted_at` goes back to `null` and new rows are added.

### Values that are normalized or chosen

- **Attributes:** these per-product fields are left out of attributes: `location`, `usagetype`, `operation`, `instancesku`, `servicecode`, `servicename`, the variant fields, and a few per-offer fields that change with the variant rather than the instance (such as ElastiCache `year_number`; see `Dropped` in `offers.go`). `productFamily` is added as an attribute.
- **usagetype variant:** for every offer but EC2, the variant's `usagetype` has the region prefix and everything from `:` on removed. So `APS3-InstanceUsage:db.r5.large` is stored as `InstanceUsage`, and one usage is one variant in every region.
- **Region attributes:** `regionCode` and `locationType` describe the location, not the instance, so they're stored once per region in `regions` instead of as overrides on every instance. Older files without `regionCode` simply leave it out for that region.
- **Disagreeing products:** when products in one region disagree on an attribute, the value most of them have is kept.
- **Spellings:** purchase options and lease lengths are cleaned up (`PartialUpfront` → `Partial Upfront`, `3 yr` → `3yr`).
- **Base price:** this is the most common on-demand price for the instance, so the most rows can leave their price out.
