package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Azure publishes its retail prices through the Retail Prices API, which needs no key but only has current
// prices, so the update script reads it on a schedule: `fetch azure` saves one read as a source file, which
// compress and append turn into a snapshot dated by when it was fetched. Each price keeps the
// effectiveStartDate Azure gives it.
//
// The API has no instance specs, so they come from the Azure pricing calculator's data (cores, RAM, temporary
// disk, GPUs, series), matched to the API's ARM SKU names.

const (
	azureRetailURL = "https://prices.azure.com/api/retail/prices"
	// The preview API version adds savingsPlan to each price.
	azureRetailVersion = "2023-01-01-preview"
	azureRetailFilter  = "serviceName eq 'Virtual Machines' and priceType ne 'DevTestConsumption'"
	azureCalculatorURL = "https://azure.microsoft.com/api/v3/pricing/virtual-machines/calculator/?culture=en-us&discount=mosp"
)

// azureSourceKind marks a fetched Azure file; compress and append look for it to recognize one.
const azureSourceKind = "azure-retail-prices"

const (
	azureRegular     = "Regular"
	azureSpot        = "Spot"
	azureLowPriority = "Low Priority"
)

// azureSpecFields are the calculator fields kept as attributes. The rest are prices or page flags.
var azureSpecFields = map[string]bool{"cores": true, "ram": true, "diskSize": true, "gpu": true, "series": true, "isVcpu": true}

var (
	// Windows products are named "... Series Windows", "... Series Windows VM" or "... Srs Win".
	azureWindows = regexp.MustCompile(`\b(Windows|Win)\b`)
	// Dedicated hosts are priced per host and Cloud Services per role instance; neither is a VM size.
	azureNotVM = regexp.MustCompile(`(?i)Dedicated ?Host|Host Reservation|Cloud ?Services`)
)

// azureSource is a fetched Azure file: the Retail Prices API's VM prices, the calculator's specs, and when
// they were read.
type azureSource struct {
	Kind      string                    `json:"kind"`
	FetchedAt string                    `json:"fetchedAt"`
	Items     []azureItem               `json:"items"`
	Specs     map[string]map[string]any `json:"specs"` // calculator offer slug -> spec fields
}

// azureItem is one Retail Prices API row, with only the fields the reader uses.
type azureItem struct {
	ArmSkuName         string             `json:"armSkuName"`
	ArmRegionName      string             `json:"armRegionName"`
	Location           string             `json:"location,omitempty"`
	ProductName        string             `json:"productName"`
	SkuName            string             `json:"skuName"`
	MeterName          string             `json:"meterName,omitempty"`
	Type               string             `json:"type"`
	ReservationTerm    string             `json:"reservationTerm,omitempty"`
	RetailPrice        float64            `json:"retailPrice"`
	CurrencyCode       string             `json:"currencyCode"`
	UnitOfMeasure      string             `json:"unitOfMeasure"`
	EffectiveStartDate string             `json:"effectiveStartDate,omitempty"`
	SavingsPlan        []azureSavingsPlan `json:"savingsPlan,omitempty"`
}

type azureSavingsPlan struct {
	RetailPrice float64 `json:"retailPrice"`
	Term        string  `json:"term"`
}

// fetchAzure reads every VM price from the Retail Prices API and the calculator's specs.
func fetchAzure(ctx context.Context, client *http.Client, retailURL, calculatorURL string) (*azureSource, error) {
	fetched := time.Now().UTC()
	items, err := fetchAzureItems(ctx, client, retailURL, 8)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("Azure Retail Prices API returned no prices")
	}

	var calc struct {
		Offers map[string]map[string]any `json:"offers"`
	}
	if err := getJSON(ctx, client, calculatorURL, &calc); err != nil {
		return nil, fmt.Errorf("Azure pricing calculator: %w", err)
	}
	specs := map[string]map[string]any{}
	for slug, o := range calc.Offers {
		spec := map[string]any{}
		for k, v := range o {
			if azureSpecFields[k] {
				spec[k] = v
			}
		}
		if len(spec) > 0 {
			specs[slug] = spec
		}
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("Azure pricing calculator returned no VM sizes")
	}
	return &azureSource{Kind: azureSourceKind, FetchedAt: fetched.Format(time.RFC3339), Items: items, Specs: specs}, nil
}

// azureSplit is a condition that splits a set of prices in two: those it matches and those it doesn't.
type azureSplit struct {
	yes, no string
	prefix  string // for an ARM SKU name prefix, the prefix
}

func azurePrefix(p string) azureSplit {
	f := fmt.Sprintf("startswith(armSkuName,'%s')", p)
	return azureSplit{yes: f, no: f + " eq false", prefix: p}
}

// azureSplits are tried in order on a set of prices with more than one page. The API rejects filters of more
// than about a dozen conditions, which these stay within, since a name prefix is only tried where it can
// match. The D and E series are about half the prices, so they are split further.
var azureSplits = []azureSplit{
	{yes: "priceType eq 'Reservation'", no: "priceType ne 'Reservation'"},
	{yes: "contains(productName,'Windows')", no: "contains(productName,'Windows') eq false"},
	{yes: "contains(skuName,'Spot')", no: "contains(skuName,'Spot') eq false"},
	{yes: "contains(skuName,'Low Priority')", no: "contains(skuName,'Low Priority') eq false"},
	azurePrefix("Standard_D"), azurePrefix("Standard_D1"), azurePrefix("Standard_D2"), azurePrefix("Standard_D4"), azurePrefix("Standard_D8"),
	azurePrefix("Standard_E"), azurePrefix("Standard_E1"), azurePrefix("Standard_E2"), azurePrefix("Standard_E4"), azurePrefix("Standard_E8"),
	azurePrefix("Standard_F"), azurePrefix("Standard_M"), azurePrefix("Standard_N"), azurePrefix("Standard_L"),
}

// azurePart is a set of prices the Retail Prices API can list with one filter.
type azurePart struct {
	filter string
	next   int      // the next split to try
	in     string   // the name prefix the part is limited to, if any
	out    []string // name prefixes the part excludes
}

// canSplit reports whether a split could divide the part, rather than leave all of it on one side.
func (p azurePart) canSplit(s azureSplit) bool {
	if s.prefix == "" {
		return true
	}
	if p.in != "" && !strings.HasPrefix(s.prefix, p.in) {
		return false
	}
	for _, o := range p.out {
		if strings.HasPrefix(s.prefix, o) {
			return false
		}
	}
	return true
}

// fetchAzureItems lists every VM price. The API pages 1000 prices at a time, each page linking to the next,
// which is about 600 pages in a row, so the prices are split into parts that are read in parallel. A part
// with more than one page is split in two by the next of azureSplits: the API has no `not` or `or`, but takes
// `... eq false`, so the two halves never overlap and never miss a price.
func fetchAzureItems(ctx context.Context, client *http.Client, retailURL string, workers int) ([]azureItem, error) {
	pageURL := func(filter string) string {
		q := url.Values{"api-version": {azureRetailVersion}, "$filter": {filter}}
		return retailURL + "?" + q.Encode()
	}
	type page struct {
		Items        []azureItem `json:"Items"`
		NextPageLink string      `json:"NextPageLink"`
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, workers)
	get := func(u string, dst *page) error {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		defer func() { <-sem }()
		return getJSON(ctx, client, u, dst)
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		results  = map[string][]azureItem{}
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	var run func(p azurePart)
	run = func(p azurePart) {
		defer wg.Done()
		var pg page
		if err := get(pageURL(p.filter), &pg); err != nil {
			fail(fmt.Errorf("Azure Retail Prices API (%s): %w", p.filter, err))
			return
		}
		if pg.NextPageLink != "" {
			for i := p.next; i < len(azureSplits); i++ {
				s := azureSplits[i]
				if !p.canSplit(s) {
					continue
				}
				yes := azurePart{filter: p.filter + " and " + s.yes, next: i + 1, in: p.in, out: p.out}
				no := azurePart{filter: p.filter + " and " + s.no, next: i + 1, in: p.in, out: slices.Clone(p.out)}
				if s.prefix != "" {
					yes.in = s.prefix
					no.out = append(no.out, s.prefix)
				}
				wg.Add(2)
				go run(yes)
				go run(no)
				return
			}
		}
		items := pg.Items
		for n := 2; pg.NextPageLink != ""; n++ {
			next := pg.NextPageLink
			pg = page{}
			if err := get(next, &pg); err != nil {
				fail(fmt.Errorf("Azure Retail Prices API (%s) page %d: %w", p.filter, n, err))
				return
			}
			items = append(items, pg.Items...)
		}
		mu.Lock()
		results[p.filter] = items
		mu.Unlock()
	}
	wg.Add(1)
	go run(azurePart{filter: azureRetailFilter})
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	var items []azureItem
	for _, f := range slices.Sorted(maps.Keys(results)) {
		items = append(items, results[f]...)
	}
	return items, nil
}

// getJSON GETs a URL and decodes the JSON answer, keeping numbers as their source text, retrying throttling
// and server errors.
func getJSON(ctx context.Context, client *http.Client, u string, dst any) error {
	return getJSONAuth(ctx, client, u, "", dst)
}

// getJSONAuth is getJSON with an OAuth access token, if token isn't empty.
func getJSONAuth(ctx context.Context, client *http.Client, u, token string, dst any) error {
	var last error
	for attempt := range 6 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "other-instances")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			last = fmt.Errorf("%s", resp.Status)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return &httpStatusError{Code: resp.StatusCode, Status: resp.Status, Body: string(bytes.TrimSpace(data[:min(len(data), 300)]))}
		}
		return decodeNumbers(data, dst)
	}
	return last
}

// httpStatusError is an answer getJSON doesn't retry, for callers that treat some statuses differently.
type httpStatusError struct {
	Code   int
	Status string
	Body   string
}

func (e *httpStatusError) Error() string { return e.Status + ": " + e.Body }

// isAzureSource reports whether the file starting with head is a fetched Azure file.
func isAzureSource(head []byte) bool {
	return sourceKind(head) == azureSourceKind
}

// readAzure turns a fetched Azure file into a snapshot.
func readAzure(r io.Reader) (*snapshot, error) {
	var src azureSource
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("decoding Azure file: %w", err)
	}
	if src.Kind != azureSourceKind {
		return nil, fmt.Errorf("not an Azure file (kind %q)", src.Kind)
	}
	ts, err := parseTS(src.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("fetchedAt: %w", err)
	}
	specs := map[string]map[string]any{}
	for slug, spec := range src.Specs {
		os, rest, ok1 := strings.Cut(slug, "-")
		i := strings.LastIndexByte(rest, '-')
		if !ok1 || i < 0 || (os != "linux" && os != "windows") {
			continue // software plans (sql-..., rhel-...) price licenses, not VM sizes
		}
		key := azureSpecKey(rest[:i], rest[i+1:])
		// Linux and Windows describe the same size; prefer Linux so the choice doesn't depend on map order.
		if _, seen := specs[key]; !seen || os == "linux" {
			specs[key] = spec
		}
	}

	s := &snapshot{Offer: azureOffer, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}}
	for _, it := range src.Items {
		if azureNotVM.MatchString(it.ProductName) {
			continue
		}
		s.Products++
		if it.ArmSkuName == "" || it.ArmRegionName == "" || it.UnitOfMeasure != "1 Hour" {
			s.Skipped++
			continue
		}
		if it.CurrencyCode != "USD" {
			return nil, fmt.Errorf("%s in %s is priced in %q, not USD", it.ArmSkuName, it.ArmRegionName, it.CurrencyCode)
		}
		region := it.ArmRegionName
		if s.Regions[region] == nil {
			locType := "Azure Region"
			if region == "Global" {
				locType = "Global" // a few Spot prices that apply in every region
			}
			s.Regions[region] = map[string]string{"regionCode": region, "locationType": locType}
		}
		if it.Location != "" {
			s.Regions[region]["name"] = it.Location
		}
		osName := "Linux"
		if azureWindows.MatchString(it.ProductName) {
			osName = "Windows"
		}
		priority := azureRegular
		switch name := it.SkuName; {
		case strings.HasSuffix(name, " Spot"):
			priority = azureSpot
		case strings.HasSuffix(name, " Low Priority"):
			priority = azureLowPriority
		}
		eff := ts
		if it.EffectiveStartDate != "" {
			e, err := parseTS(it.EffectiveStartDate)
			if err != nil {
				return nil, fmt.Errorf("%s in %s: effectiveStartDate: %w", it.ArmSkuName, region, err)
			}
			eff = min(e, ts) // a price the API lists is in effect now, whatever date it gives
		}

		si := s.Instances[it.ArmSkuName]
		if si == nil {
			si = newSnapInstance()
			s.Instances[it.ArmSkuName] = si
		}
		variant := Variant{osName, priority}
		switch it.Type {
		case "Consumption":
			s.setODAt(si, odKey{Region: region, Variant: variant}, it.RetailPrice, eff)
			for _, sp := range it.SavingsPlan {
				lease, err := azureLease(sp.Term)
				if err != nil {
					return nil, fmt.Errorf("%s in %s: savings plan: %w", it.ArmSkuName, region, err)
				}
				s.setRSAt(si, rsKey{Region: region, Variant: variant, Term: Term{lease, "Savings Plan", "No Upfront"}}, sp.RetailPrice, 0, eff)
			}
			if priority == azureRegular {
				attrs := si.Attrs[region]
				if attrs == nil {
					attrs = map[string]string{}
					si.Attrs[region] = attrs
				}
				if osName == "Linux" || attrs["skuName"] == "" {
					attrs["skuName"] = it.SkuName
				}
				if osName == "Linux" {
					attrs["productName"] = it.ProductName
				}
			}
		case "Reservation":
			lease, err := azureLease(it.ReservationTerm)
			if err != nil {
				return nil, fmt.Errorf("%s in %s: reservation: %w", it.ArmSkuName, region, err)
			}
			// A reservation costs the same paid up front or monthly; the price is the whole term's.
			s.setRSAt(si, rsKey{Region: region, Variant: variant, Term: Term{lease, "Reserved Instance", "All Upfront"}}, 0, it.RetailPrice, eff)
		default:
			s.Skipped++
		}
	}

	for name, si := range s.Instances {
		if len(si.OD) == 0 && len(si.RS) == 0 {
			delete(s.Instances, name)
			continue
		}
		spec := specs[azureArmSpecKey(name)]
		for region := range regionsOf(si) {
			attrs := si.Attrs[region]
			if attrs == nil {
				attrs = map[string]string{}
				si.Attrs[region] = attrs
			}
			attrs["productFamily"] = "Virtual Machines"
			for k, v := range spec {
				flatten(k, v, attrs)
			}
		}
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("Azure file has no VM prices")
	}
	return s, nil
}

// regionsOf lists every region an instance has a price in.
func regionsOf(si *snapInstance) map[string]bool {
	out := map[string]bool{}
	for k := range si.OD {
		out[k.Region] = true
	}
	for k := range si.RS {
		out[k.Region] = true
	}
	return out
}

// azureSpecKey matches calculator names (d2sv3, e4-2s-v3) to ARM names (Standard_D2s_v3, Standard_E4-2s_v3):
// the tier and the name's letters and digits, lowercased.
func azureSpecKey(name, tier string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(tier))
	b.WriteByte(':')
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func azureArmSpecKey(arm string) string {
	// Promo sizes are the same hardware as the size they discount, which is the one the calculator lists.
	arm = strings.TrimSuffix(arm, "_Promo")
	tier, name, ok := strings.Cut(arm, "_")
	if !ok {
		return azureSpecKey(arm, "standard")
	}
	return azureSpecKey(name, tier)
}

// azureLease names a term the way AWS bundles do: "1 Year" is "1yr", "3 Years" is "3yr".
func azureLease(term string) (string, error) {
	n, unit, ok := strings.Cut(term, " ")
	years, err := strconv.Atoi(n)
	if !ok || err != nil || years < 1 || (unit != "Year" && unit != "Years") {
		return "", fmt.Errorf("unknown term %q", term)
	}
	return strconv.Itoa(years) + "yr", nil
}
