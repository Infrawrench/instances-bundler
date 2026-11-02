package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Variant is the set of product fields that pick a price, named by the offer's VariantKeys. Unused fields are empty.
type Variant [maxVariantKeys]string

// Term is a reserved term: lease length, offering class, purchase option.
type Term [3]string

// regionAttrs describe the location rather than the instance, so they are stored once per region.
var regionAttrs = map[string]bool{"regionCode": true, "locationType": true}

// optionFix folds AWS's inconsistent purchase option spellings into one.
var optionFix = map[string]string{"PartialUpfront": "Partial Upfront", "AllUpfront": "All Upfront", "NoUpfront": "No Upfront"}

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

// snapshot is one AWS pricing file reduced to what the bundle stores.
type snapshot struct {
	Offer      string
	TS         int64
	Instances  map[string]*snapInstance
	Regions    map[string]map[string]string // location -> region attribute -> value
	Products   int
	Collisions int
}

type snapInstance struct {
	Attrs map[string]map[string]string // region -> key -> value
	OD    map[odKey]odPrice
	RS    map[rsKey]rsPrice
}

type awsProduct struct {
	ProductFamily string            `json:"productFamily"`
	Attributes    map[string]string `json:"attributes"`
}

type awsDimension struct {
	Unit         string            `json:"unit"`
	PricePerUnit map[string]string `json:"pricePerUnit"`
}

type awsOffer struct {
	EffectiveDate   string                  `json:"effectiveDate"`
	PriceDimensions map[string]awsDimension `json:"priceDimensions"`
	TermAttributes  map[string]string       `json:"termAttributes"`
}

type skuInfo struct {
	Name, Region string
	Variant      Variant
}

type rsTerm struct {
	Term  Term
	Price rsPrice
}

type awsReader struct {
	hostSuffix string
	offerCode  string
	offer      Offer
	dropped    map[string]bool
	pubDate    string
	skus       map[string]skuInfo
	attrCounts map[string]map[string]map[string]map[string]int // name -> region -> key -> value -> SKUs
	regCounts  map[string]map[string]map[string]int            // region -> key -> value -> SKUs
	od         map[string]odPrice
	rs         map[string][]rsTerm
	strs       map[string]string
}

// readAWS streams an AWS offer file, keeping only instance products and their on-demand and reserved prices.
// The file's offerCode picks how it is read, so it must come before the products, as it does in AWS files.
func readAWS(r io.Reader, hostSuffix string) (*snapshot, error) {
	a := &awsReader{
		hostSuffix: hostSuffix,
		skus:       map[string]skuInfo{},
		attrCounts: map[string]map[string]map[string]map[string]int{},
		regCounts:  map[string]map[string]map[string]int{},
		od:         map[string]odPrice{},
		rs:         map[string][]rsTerm{},
		strs:       map[string]string{},
	}
	dec := json.NewDecoder(bufio.NewReaderSize(r, 4<<20))
	err := eachKey(dec, func(key string) error {
		switch key {
		case "offerCode":
			if err := dec.Decode(&a.offerCode); err != nil {
				return err
			}
			o, err := lookupOffer(a.offerCode)
			if err != nil {
				return err
			}
			a.offer, a.dropped = o, o.dropped()
			return nil
		case "publicationDate":
			return dec.Decode(&a.pubDate)
		case "products":
			if a.dropped == nil {
				return fmt.Errorf("pricing file has no offerCode before its products")
			}
			return eachKey(dec, func(sku string) error {
				var p awsProduct
				if err := dec.Decode(&p); err != nil {
					return fmt.Errorf("product %s: %w", sku, err)
				}
				a.addProduct(sku, p)
				return nil
			})
		case "terms":
			return eachKey(dec, func(termType string) error {
				if termType != "OnDemand" && termType != "Reserved" {
					return skipValue(dec)
				}
				return eachKey(dec, func(sku string) error {
					// Products come before terms in AWS files, so terms for non-instance products can be skipped.
					if _, ok := a.skus[sku]; !ok && len(a.skus) > 0 {
						return skipValue(dec)
					}
					var offers map[string]awsOffer
					if err := dec.Decode(&offers); err != nil {
						return fmt.Errorf("%s terms for %s: %w", termType, sku, err)
					}
					return a.addOffers(termType, sku, offers)
				})
			})
		}
		return skipValue(dec)
	})
	if err != nil {
		return nil, err
	}
	return a.snapshot()
}

func (a *awsReader) intern(s string) string {
	if v, ok := a.strs[s]; ok {
		return v
	}
	a.strs[s] = s
	return s
}

func (a *awsReader) addProduct(sku string, p awsProduct) {
	name := p.Attributes["instanceType"]
	if name == "NA" && a.offer.UsageNames {
		_, name, _ = strings.Cut(p.Attributes["usagetype"], ":")
	}
	if name == "" || slices.Contains(a.offer.SkipFamilies, p.ProductFamily) {
		return
	}
	if p.ProductFamily == "Dedicated Host" {
		name += a.hostSuffix
	}
	name = a.intern(name)
	region := a.intern(p.Attributes["location"])
	var v Variant
	for i, k := range a.offer.VariantKeys {
		val := p.Attributes[k]
		if k == "usagetype" {
			val = trimUsage(val)
		}
		v[i] = a.intern(val)
	}
	a.skus[sku] = skuInfo{name, region, v}

	regions := a.attrCounts[name]
	if regions == nil {
		regions = map[string]map[string]map[string]int{}
		a.attrCounts[name] = regions
	}
	keys := regions[region]
	if keys == nil {
		keys = map[string]map[string]int{}
		regions[region] = keys
	}
	regKeys := a.regCounts[region]
	if regKeys == nil {
		regKeys = map[string]map[string]int{}
		a.regCounts[region] = regKeys
	}
	count := func(keys map[string]map[string]int, k, val string) {
		vals := keys[k]
		if vals == nil {
			vals = map[string]int{}
			keys[a.intern(k)] = vals
		}
		vals[a.intern(val)]++
	}
	for k, val := range p.Attributes {
		switch {
		case regionAttrs[k]:
			count(regKeys, k, val)
		case !a.dropped[k]:
			count(keys, k, val)
		}
	}
	count(keys, "productFamily", p.ProductFamily)
}

func (a *awsReader) addOffers(termType, sku string, offers map[string]awsOffer) error {
	for _, code := range slices.Sorted(maps.Keys(offers)) {
		o := offers[code]
		eff, err := parseTS(o.EffectiveDate)
		if err != nil {
			return fmt.Errorf("%s offer %s: %w", termType, code, err)
		}
		var hourly, upfront float64
		hasHourly := false
		for _, dk := range slices.Sorted(maps.Keys(o.PriceDimensions)) {
			d := o.PriceDimensions[dk]
			usd, ok := d.PricePerUnit["USD"]
			if !ok {
				return fmt.Errorf("%s offer %s has no USD price", termType, code)
			}
			p, err := strconv.ParseFloat(usd, 64)
			if err != nil {
				return fmt.Errorf("%s offer %s: bad price %q", termType, code, usd)
			}
			if d.Unit == "Quantity" {
				upfront = p
			} else if !hasHourly {
				hourly, hasHourly = p, true
			}
		}
		if !hasHourly {
			return fmt.Errorf("%s offer %s has no hourly price", termType, code)
		}
		if termType == "OnDemand" {
			if prev, ok := a.od[sku]; !ok || eff >= prev.Eff {
				a.od[sku] = odPrice{eff, hourly}
			}
			continue
		}
		ta := o.TermAttributes
		opt := ta["PurchaseOption"]
		if fixed, ok := optionFix[opt]; ok {
			opt = fixed
		}
		term := Term{a.intern(strings.ReplaceAll(ta["LeaseContractLength"], " ", "")), a.intern(ta["OfferingClass"]), a.intern(opt)}
		a.rs[sku] = append(a.rs[sku], rsTerm{term, rsPrice{eff, hourly, upfront}})
	}
	return nil
}

func (a *awsReader) snapshot() (*snapshot, error) {
	if a.offerCode == "" {
		return nil, fmt.Errorf("pricing file has no offerCode")
	}
	if a.pubDate == "" {
		return nil, fmt.Errorf("pricing file has no publicationDate")
	}
	ts, err := parseTS(a.pubDate)
	if err != nil {
		return nil, fmt.Errorf("publicationDate: %w", err)
	}
	s := &snapshot{Offer: a.offerCode, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}, Products: len(a.skus)}
	for region, keys := range a.regCounts {
		if len(keys) == 0 {
			continue
		}
		attrs := make(map[string]string, len(keys))
		for k, vals := range keys {
			attrs[k] = mostCommon(vals)
		}
		s.Regions[region] = attrs
	}
	for name, regions := range a.attrCounts {
		si := &snapInstance{Attrs: map[string]map[string]string{}, OD: map[odKey]odPrice{}, RS: map[rsKey]rsPrice{}}
		for region, keys := range regions {
			attrs := make(map[string]string, len(keys))
			for k, vals := range keys {
				attrs[k] = mostCommon(vals)
			}
			si.Attrs[region] = attrs
		}
		s.Instances[name] = si
	}
	for sku, p := range a.od {
		info, ok := a.skus[sku]
		if !ok {
			continue
		}
		k := odKey{info.Region, info.Variant}
		od := s.Instances[info.Name].OD
		if _, dup := od[k]; dup {
			s.Collisions++
		}
		od[k] = p
	}
	for sku, terms := range a.rs {
		info, ok := a.skus[sku]
		if !ok {
			continue
		}
		rs := s.Instances[info.Name].RS
		for _, t := range terms {
			k := rsKey{info.Region, info.Variant, t.Term}
			if _, dup := rs[k]; dup {
				s.Collisions++
			}
			rs[k] = t.Price
		}
	}
	return s, nil
}

// mostCommon picks the value most SKUs agree on, breaking ties by the smaller string.
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

func eachKey(dec *json.Decoder, fn func(key string) error) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := t.(string)
		if !ok {
			return fmt.Errorf("expected object key, got %v", t)
		}
		if err := fn(key); err != nil {
			return err
		}
	}
	return expectDelim(dec, '}')
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, got %v", want, t)
	}
	return nil
}

func skipValue(dec *json.Decoder) error {
	var skip json.RawMessage
	return dec.Decode(&skip)
}
