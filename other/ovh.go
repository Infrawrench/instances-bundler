package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// OVHcloud's order catalog (/order/catalog/public/...) lists what it sells without a login, in the currency of
// the OVHcloud subsidiary asked for. The World subsidiary (WS) sells in USD, and OVHcloud US, which has its own
// API and prices, does too. Neither has any history, so the update scripts read them on a schedule.
//
// `fetch ovh` saves Public Cloud instance flavors from both. `fetch ovheco` saves the dedicated servers of the
// "eco" ranges (Kimsufi, So you Start, Rise and Advance). OVHcloud has no server auction: eco servers are the
// closest it has, at fixed prices.

const (
	ovhWorldURL = "https://ca.api.ovh.com/v1"
	ovhUSURL    = "https://api.us.ovhcloud.com/v1"
)

const (
	ovhSourceKind    = "ovh-public-cloud-catalog"
	ovhEcoSourceKind = "ovh-eco-catalog"
)

// ovhPriceScale is what the catalog's integer prices are multiplied by.
const ovhPriceScale = 1e8

type ovhPricing struct {
	Capacities   []string `json:"capacities"`
	Commitment   int      `json:"commitment"`
	Interval     int      `json:"interval"`
	IntervalUnit string   `json:"intervalUnit"`
	Price        float64  `json:"price"`
	Mode         string   `json:"mode"`
}

func (p ovhPricing) dollars() float64 { return roundPrice(p.Price / ovhPriceScale) }

type ovhLocale struct {
	CurrencyCode string `json:"currencyCode"`
	Subsidiary   string `json:"subsidiary"`
}

// ovhFlavor is a Public Cloud addon: one instance flavor, one way of paying for it, in one pricing zone.
type ovhFlavor struct {
	PlanCode string         `json:"planCode"`
	Product  string         `json:"product,omitempty"`
	Pricings []ovhPricing   `json:"pricings"`
	Blobs    map[string]any `json:"blobs,omitempty"`
}

type ovhCatalog struct {
	Locale ovhLocale   `json:"locale"`
	Addons []ovhFlavor `json:"addons"`
}

type ovhSource struct {
	Kind      string       `json:"kind"`
	FetchedAt string       `json:"fetchedAt"`
	Catalogs  []ovhCatalog `json:"catalogs"`
}

// ovhFlavorProduct marks a catalog addon as an instance flavor; the rest are databases, volumes and so on.
const ovhFlavorProduct = "publiccloud-instance"

// fetchOVH reads the instance flavors of the World and US catalogs.
func fetchOVH(ctx context.Context, client *http.Client, worldURL, usURL string) (*ovhSource, error) {
	fetched := time.Now().UTC()
	src := &ovhSource{Kind: ovhSourceKind, FetchedAt: fetched.Format(time.RFC3339)}
	for _, c := range []struct{ base, subsidiary string }{{worldURL, "WS"}, {usURL, "US"}} {
		var cat ovhCatalog
		u := fmt.Sprintf("%s/order/catalog/public/cloud?ovhSubsidiary=%s", c.base, c.subsidiary)
		if err := getJSON(ctx, client, u, &cat); err != nil {
			return nil, fmt.Errorf("OVHcloud %s catalog: %w", c.subsidiary, err)
		}
		cat.Addons = slices.DeleteFunc(cat.Addons, func(a ovhFlavor) bool { return a.Product != ovhFlavorProduct })
		if len(cat.Addons) == 0 {
			return nil, fmt.Errorf("OVHcloud %s catalog has no instance flavors", c.subsidiary)
		}
		src.Catalogs = append(src.Catalogs, cat)
	}
	return src, nil
}

// ovhFlavorCode reads a flavor's plan code: "b3-8.consumption", "b3-8.consumption.LZ.AF", "b2-7.monthly.postpaid".
var ovhFlavorCode = regexp.MustCompile(`^(.+?)\.(consumption|monthly)(?:\.(.+))?$`)

// ovhZoneNames describes the pricing zones. The catalog prices flavors by zone, not region, and the list of
// regions in each zone needs a login, so the zones are what the regions table has.
var ovhZoneNames = map[string]string{
	"world":            "Standard regions",
	"3az":              "3-AZ regions",
	"lz-eu":            "Local Zones in Europe",
	"lz-eurozone":      "Local Zones in the eurozone",
	"lz-af":            "Local Zones in Africa",
	"us":               "OVHcloud US regions",
	"us-east-lz-dal-a": "OVHcloud US Local Zone in Dallas",
}

// ovhZone names a flavor's pricing zone from its plan code's suffix: the World catalog's zones, or the US's.
func ovhZone(subsidiary, suffix string) string {
	zone := strings.ToLower(strings.ReplaceAll(suffix, ".", "-"))
	switch {
	case subsidiary == "US" && zone == "":
		return "us"
	case subsidiary == "US" && !strings.HasPrefix(zone, "us"):
		return "us-" + zone
	case zone == "":
		return "world"
	}
	return zone
}

// readOVH turns a fetched OVHcloud Public Cloud file into a snapshot.
func readOVH(r io.Reader) (*snapshot, error) {
	var src ovhSource
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("decoding OVHcloud file: %w", err)
	}
	if src.Kind != ovhSourceKind {
		return nil, fmt.Errorf("not an OVHcloud file (kind %q)", src.Kind)
	}
	ts, err := parseTS(src.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("fetchedAt: %w", err)
	}
	s := &snapshot{Offer: ovhOffer, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}}
	// A flavor's specs are the same in every zone. Linux flavors' come first because Windows ones repeat them.
	specs := map[string]map[string]string{}
	for _, cat := range src.Catalogs {
		if cat.Locale.CurrencyCode != "USD" {
			return nil, fmt.Errorf("OVHcloud %s catalog is priced in %q, not USD", cat.Locale.Subsidiary, cat.Locale.CurrencyCode)
		}
		for _, a := range cat.Addons {
			m := ovhFlavorCode.FindStringSubmatch(a.PlanCode)
			s.Products++
			if m == nil {
				s.Skipped++
				continue
			}
			name, kind, suffix := m[1], m[2], m[3]
			osName := "Linux"
			if rest, ok := strings.CutPrefix(name, "win-"); ok {
				name, osName = rest, "Windows"
			}
			// "monthly.postpaid" is paid at the end of the month, and US "monthly" at its start.
			purchase := "All Upfront"
			if kind == "monthly" && suffix == "postpaid" {
				suffix, purchase = "", "Postpaid"
			}
			zone := ovhZone(cat.Locale.Subsidiary, suffix)
			wantUnit := "hour"
			if kind == "monthly" {
				wantUnit = "month"
			}
			var price *ovhPricing
			for i, p := range a.Pricings {
				// The US catalog gives metered prices no interval: its plan code alone says what they are per.
				metered := p.IntervalUnit == "none" && slices.Contains(p.Capacities, "consumption")
				if p.Mode == "default" && (metered || (p.IntervalUnit == wantUnit && p.Interval == 1)) {
					price = &a.Pricings[i]
					break
				}
			}
			if price == nil {
				s.Skipped++
				continue
			}
			if s.Regions[zone] == nil {
				s.Regions[zone] = map[string]string{"regionCode": zone, "locationType": "OVHcloud Pricing Zone"}
				if n, ok := ovhZoneNames[zone]; ok {
					s.Regions[zone]["name"] = n
				}
			}
			si := s.Instances[name]
			if si == nil {
				si = newSnapInstance()
				s.Instances[name] = si
			}
			variant := Variant{osName}
			if kind == "consumption" {
				s.setOD(si, odKey{Region: zone, Variant: variant}, price.dollars())
			} else {
				s.setRS(si, rsKey{Region: zone, Variant: variant, Term: Term{"1mo", "Monthly", purchase}}, 0, price.dollars())
			}
			if specs[name] == nil || osName == "Linux" {
				if attrs := ovhFlavorAttrs(a.Blobs); attrs != nil {
					specs[name] = attrs
				}
			}
		}
	}
	for name, si := range s.Instances {
		attrs := specs[name]
		if attrs == nil {
			attrs = map[string]string{}
		}
		attrs["productFamily"] = "Public Cloud Instance"
		si.Attrs[""] = attrs
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("OVHcloud file has no prices")
	}
	return s, nil
}

// ovhFlavorAttrs flattens a flavor's specs, or returns nil if it has none. The name is the instance's, and the
// operating system is the variant's.
func ovhFlavorAttrs(blobs map[string]any) map[string]string {
	tech, ok := blobs["technical"].(map[string]any)
	if !ok {
		return nil
	}
	attrs := map[string]string{}
	flatten("", tech, attrs)
	delete(attrs, "name")
	delete(attrs, "os.family")
	if commercial, ok := blobs["commercial"].(map[string]any); ok {
		if brick, ok := commercial["brick"].(string); ok {
			attrs["brick"] = brick
		}
		if sub, ok := commercial["brickSubtype"].(string); ok && sub != "" {
			attrs["brickSubtype"] = sub
		}
	}
	return attrs
}

// --- Eco dedicated servers ---

type ovhEcoFamily struct {
	Name      string   `json:"name"`
	Mandatory bool     `json:"mandatory"`
	Default   string   `json:"default"`
	Addons    []string `json:"addons"`
}

type ovhEcoConfig struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// ovhEcoPlan is a server sold in some set of data centers. A server sold in another set at another price has
// another plan, with the same product.
type ovhEcoPlan struct {
	PlanCode       string         `json:"planCode"`
	InvoiceName    string         `json:"invoiceName"`
	Product        string         `json:"product"`
	Pricings       []ovhPricing   `json:"pricings"`
	AddonFamilies  []ovhEcoFamily `json:"addonFamilies"`
	Configurations []ovhEcoConfig `json:"configurations"`
}

type ovhEcoAddon struct {
	PlanCode string       `json:"planCode"`
	Pricings []ovhPricing `json:"pricings"`
}

type ovhEcoProduct struct {
	Name  string         `json:"name"`
	Blobs map[string]any `json:"blobs,omitempty"`
}

// ovhEcoOption is a configuration OVHcloud sells a plan in, as its availability list names it.
type ovhEcoOption struct {
	PlanCode      string `json:"planCode"`
	Memory        string `json:"memory"`
	Storage       string `json:"storage"`
	SystemStorage string `json:"systemStorage,omitempty"`
}

type ovhEcoSource struct {
	Kind      string          `json:"kind"`
	FetchedAt string          `json:"fetchedAt"`
	Locale    ovhLocale       `json:"locale"`
	Plans     []ovhEcoPlan    `json:"plans"`
	Addons    []ovhEcoAddon   `json:"addons"`
	Products  []ovhEcoProduct `json:"products"`
	Options   []ovhEcoOption  `json:"options"`
}

// fetchOVHEco reads the eco catalog and which configurations its servers are sold in. Stock levels are left out:
// they change too often to be history.
func fetchOVHEco(ctx context.Context, client *http.Client, base string) (*ovhEcoSource, error) {
	fetched := time.Now().UTC()
	var cat struct {
		Locale   ovhLocale       `json:"locale"`
		Plans    []ovhEcoPlan    `json:"plans"`
		Addons   []ovhEcoAddon   `json:"addons"`
		Products []ovhEcoProduct `json:"products"`
	}
	if err := getJSON(ctx, client, base+"/order/catalog/public/eco?ovhSubsidiary=WS", &cat); err != nil {
		return nil, fmt.Errorf("OVHcloud eco catalog: %w", err)
	}
	if len(cat.Plans) == 0 {
		return nil, fmt.Errorf("OVHcloud eco catalog has no servers")
	}
	var options []ovhEcoOption
	if err := getJSON(ctx, client, base+"/dedicated/server/datacenter/availabilities", &options); err != nil {
		return nil, fmt.Errorf("OVHcloud availabilities: %w", err)
	}
	plans := map[string]bool{}
	for _, p := range cat.Plans {
		plans[p.PlanCode] = true
	}
	options = slices.DeleteFunc(options, func(o ovhEcoOption) bool { return !plans[o.PlanCode] })
	// The catalog also lists every license and so on; only the addons a server's price can include are kept.
	used := map[string]bool{}
	for _, p := range cat.Plans {
		for _, f := range p.AddonFamilies {
			if f.Mandatory {
				for _, a := range f.Addons {
					used[a] = true
				}
			}
		}
	}
	cat.Addons = slices.DeleteFunc(cat.Addons, func(a ovhEcoAddon) bool { return !used[a.PlanCode] })
	return &ovhEcoSource{Kind: ovhEcoSourceKind, FetchedAt: fetched.Format(time.RFC3339), Locale: cat.Locale,
		Plans: cat.Plans, Addons: cat.Addons, Products: cat.Products, Options: options}, nil
}

// ovhEcoAddonFor finds a family's addon for an option's name: availability says "ram-32g-ecc-2133" and the
// catalog "ram-32g-ecc-2133-24sk20".
func ovhEcoAddonFor(f ovhEcoFamily, option string) (string, bool) {
	for _, a := range f.Addons {
		if a == option || strings.HasPrefix(a, option+"-") {
			return a, true
		}
	}
	return "", false
}

// ovhEcoPrice is the price of a plan and its addons in one pricing mode, for the renewal (per term) or the
// installation (once).
func ovhEcoPrice(pricings [][]ovhPricing, capacity, mode string) (float64, int, bool) {
	total, months := 0.0, 0
	for _, list := range pricings {
		found := false
		for _, p := range list {
			if p.Mode == mode && slices.Contains(p.Capacities, capacity) {
				total += p.Price
				if p.IntervalUnit == "month" {
					months = max(months, p.Interval)
				}
				found = true
				break
			}
		}
		if !found {
			return 0, 0, false
		}
	}
	return roundPrice(total / ovhPriceScale), months, true
}

// ovhEcoRentTerm is a month's rent, which OVHcloud charges at the start of the month.
var ovhEcoRentTerm = Term{"1mo", "Rental", "All Upfront"}

// readOVHEco turns a fetched eco catalog file into a snapshot.
func readOVHEco(r io.Reader) (*snapshot, error) {
	var src ovhEcoSource
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("decoding OVHcloud eco file: %w", err)
	}
	if src.Kind != ovhEcoSourceKind {
		return nil, fmt.Errorf("not an OVHcloud eco file (kind %q)", src.Kind)
	}
	if src.Locale.CurrencyCode != "USD" {
		return nil, fmt.Errorf("OVHcloud eco catalog is priced in %q, not USD", src.Locale.CurrencyCode)
	}
	ts, err := parseTS(src.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("fetchedAt: %w", err)
	}
	addons := map[string][]ovhPricing{}
	for _, a := range src.Addons {
		addons[a.PlanCode] = a.Pricings
	}
	products := map[string]map[string]any{}
	for _, p := range src.Products {
		products[p.Name] = p.Blobs
	}
	options := map[string][]ovhEcoOption{}
	for _, o := range src.Options {
		options[o.PlanCode] = append(options[o.PlanCode], o)
	}

	s := &snapshot{Offer: ovhEcoOffer, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}}
	for _, plan := range src.Plans {
		var datacenters []string
		for _, c := range plan.Configurations {
			if c.Name == "dedicated_datacenter" {
				datacenters = c.Values
			}
		}
		if len(options[plan.PlanCode]) == 0 || len(datacenters) == 0 {
			s.Unpriced++
			continue
		}
		si := s.Instances[plan.Product]
		if si == nil {
			si = newSnapInstance()
		}
		// Addons every configuration has, such as the included bandwidth, by family.
		var mandatory [][]ovhPricing
		families := map[string]ovhEcoFamily{}
		for _, f := range plan.AddonFamilies {
			families[f.Name] = f
			if f.Mandatory && f.Name != "memory" && f.Name != "storage" && f.Name != "system-storage" && f.Default != "" {
				mandatory = append(mandatory, addons[f.Default])
			}
		}
		priced := false
		for _, o := range options[plan.PlanCode] {
			s.Products++
			pricings := append([][]ovhPricing{plan.Pricings}, mandatory...)
			ok := true
			for _, part := range []struct{ family, option string }{{"memory", o.Memory}, {"storage", o.Storage}, {"system-storage", o.SystemStorage}} {
				if part.option == "" {
					continue
				}
				a, found := ovhEcoAddonFor(families[part.family], part.option)
				if !found {
					ok = false
					break
				}
				pricings = append(pricings, addons[a])
			}
			monthly, _, hasMonthly := ovhEcoPrice(pricings, "renew", "default")
			if !ok || !hasMonthly {
				s.Skipped++
				continue
			}
			priced = true
			variant := Variant{o.Memory, o.Storage, o.SystemStorage}
			for _, dc := range datacenters {
				// Unlike every other offer, these have no on-demand rows: the servers can only be rented by the month,
				// and an hourly price would be a number OVHcloud doesn't charge. The month's rent is a term.
				s.setRS(si, rsKey{Region: dc, Variant: variant, Term: ovhEcoRentTerm}, 0, monthly)
				for _, mode := range []string{"upfront12", "upfront24"} {
					total, months, ok := ovhEcoPrice(pricings, "renew", mode)
					if !ok || months < 12 {
						continue
					}
					term := Term{strconv.Itoa(months/12) + "yr", "Prepaid", "All Upfront"}
					s.setRS(si, rsKey{Region: dc, Variant: variant, Term: term}, 0, total)
				}
			}
		}
		if !priced {
			s.Unpriced++
			continue
		}
		s.Instances[plan.Product] = si

		attrs := map[string]string{"billing": "Monthly", "productFamily": "Dedicated Server"}
		if tech, ok := products[plan.Product]["technical"].(map[string]any); ok {
			flatten("", tech["server"], attrs)
		}
		if setup, _, ok := ovhEcoPrice([][]ovhPricing{plan.Pricings}, "installation", "default"); ok {
			attrs["setupFee"] = strconv.FormatFloat(setup, 'f', -1, 64)
		}
		attrs["invoiceName"] = plan.InvoiceName
		attrs["planCode"] = plan.PlanCode
		for _, dc := range datacenters {
			regionAttrs := map[string]string{}
			for k, v := range attrs {
				regionAttrs[k] = v
			}
			si.Attrs[dc] = regionAttrs
			if s.Regions[dc] == nil {
				s.Regions[dc] = map[string]string{"regionCode": dc, "locationType": "OVHcloud Data Center"}
				if n, ok := ovhDatacenterNames[dc]; ok {
					s.Regions[dc]["name"] = n
				}
			}
		}
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("OVHcloud eco file has no prices")
	}
	return s, nil
}

// ovhDatacenterNames are the cities of OVHcloud's data centers. Others just have no name.
var ovhDatacenterNames = map[string]string{
	"bhs": "Beauharnois",
	"fra": "Frankfurt",
	"gra": "Gravelines",
	"lon": "London",
	"rbx": "Roubaix",
	"sbg": "Strasbourg",
	"sgp": "Singapore",
	"syd": "Sydney",
	"waw": "Warsaw",
	"ynm": "Mumbai",
}
