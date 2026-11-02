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
)

// maxVariantKeys is the most VariantKeys any offer has.
const maxVariantKeys = 4

var offers = map[string]Offer{
	// GPU and CPU pods. Prices are the same in every data center, so every row is in "global".
	runpodOffer: {Platform: "runpod", VariantKeys: []string{"cloudType"}, Polled: true},
	// ECS instance types, priced per region. Old versions also price the classic network.
	alibabaOffer: {Platform: "alibaba", VariantKeys: []string{"operatingSystem", "networkType"}},
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

// setOD records an on-demand price, counting a collision if the key already had a different one.
func (s *snapshot) setOD(si *snapInstance, k odKey, p float64) {
	if prev, dup := si.OD[k]; dup && prev.Price != p {
		s.Collisions++
	}
	si.OD[k] = odPrice{Eff: s.TS, Price: p}
}

// setRS records a committed price, counting a collision if the key already had a different one.
func (s *snapshot) setRS(si *snapInstance, k rsKey, hourly, upfront float64) {
	if prev, dup := si.RS[k]; dup && (prev.Hourly != hourly || prev.Upfront != upfront) {
		s.Collisions++
	}
	si.RS[k] = rsPrice{Eff: s.TS, Hourly: hourly, Upfront: upfront}
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
