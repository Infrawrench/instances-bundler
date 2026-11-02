package main

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Offer says how to read one AWS offer's price list. Every supported offer prices products by instanceType.
type Offer struct {
	// VariantKeys are the product fields that pick a price, in the order variants store them.
	// usagetype is cut down to its usage (see trimUsage), so the same usage is one variant in every region.
	VariantKeys []string
	// SkipFamilies are product families that have an instanceType but are not priced per instance hour.
	SkipFamilies []string
	// Dropped are fields that change with the variant rather than describing the instance.
	Dropped []string
	// UsageNames takes the instance type from the usagetype, after ":", when instanceType is "NA".
	UsageNames bool
}

// defaultOffer is the offer of bundles that don't record one: EC2 bundles leave it out so their format is unchanged.
const defaultOffer = "AmazonEC2"

// maxVariantKeys is the most VariantKeys any offer has.
const maxVariantKeys = 8

// The keys were picked so no two products in one region share a variant in any published version, except
// for a few AWS mislabels in old SageMaker files (such as Stockholm prices listed under Frankfurt).
var offers = map[string]Offer{
	"AmazonEC2": {VariantKeys: []string{"operatingSystem", "tenancy", "licenseModel", "preInstalledSw", "capacitystatus"}},
	"AmazonRDS": {
		// engineCode tells apart engines with the same name, such as PostgreSQL 14 and 220 in 2019-20.
		VariantKeys: []string{"databaseEngine", "databaseEdition", "licenseModel", "deploymentOption", "deploymentModel", "engineMediaType", "engineCode", "usagetype"},
		Dropped:     []string{"normalizationSizeFactor"},
	},
	"AmazonElastiCache": {
		VariantKeys: []string{"cacheEngine", "usagetype"},
		Dropped:     []string{"normalizationSizeFactor", "year_number"},
	},
	"AmazonES": {VariantKeys: []string{"usagetype"}},
	"AmazonRedshift": {
		VariantKeys:  []string{"usagetype"},
		SkipFamilies: []string{"Redshift Concurrency Scaling", "Redshift Managed Storage"},
	},
	"AmazonDocDB": {
		VariantKeys:  []string{"usagetype"},
		SkipFamilies: []string{"CPU Credits"},
		Dropped:      []string{"storageType", "volume_optimization", "volumeType", "year_number", "engine_major_version"},
	},
	"AmazonNeptune": {
		// Versions from late 2024 to mid 2025 spell m-ncu as mncu.
		VariantKeys: []string{"m-ncu", "mncu", "usagetype"},
		Dropped:     []string{"normalizationSizeFactor", "volumeType"},
	},
	"AmazonMemoryDB": {
		VariantKeys: []string{"engine", "usagetype"},
		Dropped:     []string{"normalizationSizeFactor"},
	},
	"AmazonDAX": {VariantKeys: []string{"usagetype"}},
	"AmazonSageMaker": {
		// platoinstancetype tells apart Cluster and Cluster-Reserved.
		VariantKeys:  []string{"component", "platoinstancetype", "usagetype"},
		SkipFamilies: []string{"Data Transfer"},
		Dropped:      []string{"platoinstancename"},
		// Geospatial processing listed instanceType as NA until 2025, with the type only in the usagetype.
		UsageNames: true,
	},
	"ElasticMapReduce": {VariantKeys: []string{"softwareType", "usagetype"}},
	// operation tells apart FleetIQ and GameLift-managed instances.
	"AmazonGameLift": {VariantKeys: []string{"operatingSystem", "isSpotInstance", "operation", "usagetype"}},
	// EC2 spot prices, read from AWS's spot price feed rather than a price list (see spot.go).
	spotOffer: {VariantKeys: []string{"operatingSystem"}},
}

// offerAttrs are per-SKU identifiers and the location; none of them describe the instance itself.
var offerAttrs = []string{"instanceType", "location", "usagetype", "operation", "instancesku", "servicecode", "servicename"}

func lookupOffer(code string) (Offer, error) {
	o, ok := offers[code]
	if !ok {
		return Offer{}, fmt.Errorf("offer %q is not supported; supported offers: %s", code, strings.Join(slices.Sorted(maps.Keys(offers)), ", "))
	}
	return o, nil
}

// dropped is every field left out of an instance's attributes.
func (o Offer) dropped() map[string]bool {
	m := map[string]bool{}
	for _, keys := range [][]string{offerAttrs, o.VariantKeys, o.Dropped} {
		for _, k := range keys {
			m[k] = true
		}
	}
	return m
}

// usageRegion matches the region codes AWS puts before a usagetype, such as "USE2-" or "USW2-LAX1-".
var usageRegion = regexp.MustCompile(`^([A-Z]{2,}[0-9]*-)+`)

// trimUsage cuts a usagetype down to what it says about the price: "APS3-InstanceUsage:db.r5.large" is "InstanceUsage".
func trimUsage(usagetype string) string {
	u, _, _ := strings.Cut(usagetype, ":")
	return usageRegion.ReplaceAllString(u, "")
}
