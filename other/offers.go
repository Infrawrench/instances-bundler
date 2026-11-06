package main

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Offer is one provider's product, stored as one bundle. Each has its own variant keys and its own reader
// (runpod.go, alibaba.go); the bundle around them is the same for all of them, and the same as
// instances-bundler's, so the bulk-instances SDKs read these bundles as they read the AWS ones.
type Offer struct {
	// Platform names the bundle in the bucket (<platform>-latest, <platform>-bundle-<ts>.json.xz) and in the SDKs.
	Platform string
	// VariantKeys are the fields that pick a price, in the order variants store them.
	VariantKeys []string
	// Polled offers come from a live API with no publication date, read whenever the update script runs.
	// A read that changes nothing is not recorded, so the snapshots list only grows when something changed.
	Polled bool
}

const (
	runpodOffer  = "RunpodPods"
	alibabaOffer = "AlibabaECS"
	azureOffer   = "AzureVM"
	gcpOffer     = "GCPCompute"

	hetznerOffer        = "HetznerCloud"
	hetznerAuctionOffer = "HetznerAuction"
	ovhOffer            = "OVHPublicCloud"
	ovhEcoOffer         = "OVHEco"
	oracleOffer         = "OracleCompute"
	ibmOffer            = "IBMVPC"
)

// maxVariantKeys is the most VariantKeys any offer has.
const maxVariantKeys = 4

var offers = map[string]Offer{
	// GPU and CPU pods. Prices are the same in every data center, so every row is in "global".
	runpodOffer: {Platform: "runpod", VariantKeys: []string{"cloudType"}, Polled: true},
	// ECS instance types, priced per region. Old versions also price the classic network.
	alibabaOffer: {Platform: "alibaba", VariantKeys: []string{"operatingSystem", "networkType"}},
	// Virtual machine sizes, priced per region, with Spot and Low Priority VMs as their own priority.
	azureOffer: {Platform: "azure", VariantKeys: []string{"operatingSystem", "priority"}, Polled: true},
	// Compute Engine machine types, priced per region, with Spot VMs as their own provisioning model.
	gcpOffer: {Platform: "gcp", VariantKeys: []string{"provisioningModel"}, Polled: true},
	// Cloud servers, priced per location, with or without their primary IPv4 (which is optional and billed apart).
	hetznerOffer: {Platform: "hetzner", VariantKeys: []string{"ipv4"}, Polled: true},
	// Server Auction listings by id, each one physical server, priced like cloud servers.
	hetznerAuctionOffer: {Platform: "hetznerauction", VariantKeys: []string{"ipv4"}, Polled: true},
	// Public Cloud instance flavors, priced per pricing zone, since the catalog has no regions.
	ovhOffer: {Platform: "ovh", VariantKeys: []string{"operatingSystem"}, Polled: true},
	// Kimsufi, So you Start, Rise and Advance dedicated servers, priced per month per data center and configuration.
	ovhEcoOffer: {Platform: "ovheco", VariantKeys: []string{"memory", "storage", "systemStorage"}, Polled: true},
	// VM and bare metal shapes, priced the same everywhere. Flexible shapes are priced at a set of sizes.
	oracleOffer: {Platform: "oracle", VariantKeys: []string{"operatingSystem"}, Polled: true},
	// Virtual Server for VPC profiles, priced per region, with Spot and burstable capacity as their own type.
	ibmOffer: {Platform: "ibm", VariantKeys: []string{"capacityType"}, Polled: true},
}

func lookupOffer(code string) (Offer, error) {
	o, ok := offers[code]
	if !ok {
		return Offer{}, fmt.Errorf("offer %q is not supported; supported offers: %s", code, strings.Join(slices.Sorted(maps.Keys(offers)), ", "))
	}
	return o, nil
}

// Variant is the set of fields that pick a price, named by the offer's VariantKeys. Unused fields are empty.
type Variant [maxVariantKeys]string

// Term is a commitment: lease length, offering class, purchase option (the fields of an AWS reserved term).
type Term [3]string

type odKey struct {
	Region  string
	Variant Variant
}

type rsKey struct {
	Region  string
	Variant Variant
	Term    Term
}

type odPrice struct {
	Eff   int64
	Price float64
}

type rsPrice struct {
	Eff             int64
	Hourly, Upfront float64
}

// snapshot is one read of a provider's prices, reduced to what the bundle stores.
type snapshot struct {
	Offer string
	TS    int64
	// Version is the provider's own name for this publication, if it has one (Alibaba's "2.0.426").
	Version    string
	Instances  map[string]*snapInstance
	Regions    map[string]map[string]string // location -> region attribute -> value
	Products   int
	Collisions int
	Skipped    int // products left out because the bundle has no way to store them
	Unknown    int // prices the source has an error in place of; the bundle keeps what it had
	Unpriced   int // instance types the source lists but that have no price anywhere
}

type snapInstance struct {
	Attrs map[string]map[string]string // region -> key -> value
	OD    map[odKey]odPrice
	RS    map[rsKey]rsPrice
	// KeepRS are keys the source lists without a usable price; whatever the bundle has for them stays.
	KeepRS map[rsKey]bool
}

func newSnapInstance() *snapInstance {
	return &snapInstance{Attrs: map[string]map[string]string{}, OD: map[odKey]odPrice{}, RS: map[rsKey]rsPrice{}, KeepRS: map[rsKey]bool{}}
}

// setOD records an on-demand price that took effect when the snapshot was published.
func (s *snapshot) setOD(si *snapInstance, k odKey, p float64) { s.setODAt(si, k, p, s.TS) }

// setRS records a committed price that took effect when the snapshot was published.
func (s *snapshot) setRS(si *snapInstance, k rsKey, hourly, upfront float64) {
	s.setRSAt(si, k, hourly, upfront, s.TS)
}

// setODAt records an on-demand price that took effect at eff, counting a collision if the key already had a
// different one.
func (s *snapshot) setODAt(si *snapInstance, k odKey, p float64, eff int64) {
	if prev, dup := si.OD[k]; dup && prev.Price != p {
		s.Collisions++
	}
	si.OD[k] = odPrice{Eff: eff, Price: p}
}

// setRSAt records a committed price that took effect at eff, counting a collision if the key already had a
// different one.
func (s *snapshot) setRSAt(si *snapInstance, k rsKey, hourly, upfront float64, eff int64) {
	if prev, dup := si.RS[k]; dup && (prev.Hourly != hourly || prev.Upfront != upfront) {
		s.Collisions++
	}
	si.RS[k] = rsPrice{Eff: eff, Hourly: hourly, Upfront: upfront}
}

var (
	// errNothingNew is returned when a read has nothing to add to the bundle; the CLI exits with status 3.
	errNothingNew = errors.New("nothing new")
	errNotNewer   = fmt.Errorf("%w: prices are not newer than the bundle", errNothingNew)
	errUnchanged  = fmt.Errorf("%w: prices have not changed since the bundle's latest snapshot", errNothingNew)
)

func mostCommon(vals map[string]int) string {
	best, n := "", -1
	for v, c := range vals {
		if c > n || (c == n && v < best) {
			best, n = v, c
		}
	}
	return best
}

func parseTS(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

func fmtTS(ts int64) string {
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}
