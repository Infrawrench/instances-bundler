package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hetzner's Cloud API needs a token and prices in the account's currency, so prices come from the public API
// behind Hetzner's website (website-price-api.hetzner.com), which needs no key and answers in USD and EUR.
// It identifies a server only by a product id (CLOUD_132), so the server names, specs and product ids come from
// the rows of the website's cloud server pages. `fetch hetzner` saves both, with when they were read.
//
// `fetch hetznerauction` saves the Server Auction's listings, from the JSON file behind its page. Each listing
// is one physical server, so a server's history starts when it is listed and ends when it is sold.

const (
	hetznerPriceURL   = "https://website-price-api.hetzner.com/api/v1"
	hetznerSiteURL    = "https://www.hetzner.com"
	hetznerAuctionURL = "https://www.hetzner.com/_resources/app/data/app/live_data_sb.json"
)

const (
	hetznerSourceKind        = "hetzner-cloud-prices"
	hetznerAuctionSourceKind = "hetzner-server-auction"
)

const (
	hetznerWithIPv4 = "Primary IPv4"
	hetznerIPv6Only = "IPv6 only"
)

// hetznerPages are the website's cloud server pages, by the category their servers get.
var hetznerPages = []struct{ path, category string }{
	{"/cloud/cost-optimized/", "Cost-Optimized"},
	{"/cloud/regular-performance/", "Regular Performance"},
	{"/cloud/general-purpose/", "General Purpose"},
}

// hetznerLocations gives each of the price API's data centers its Cloud API location name and its city.
var hetznerLocations = map[string]struct{ code, name string }{
	"FSN1": {"fsn1", "Falkenstein"},
	"NBG1": {"nbg1", "Nuremberg"},
	"HEL1": {"hel1", "Helsinki"},
	"ASH1": {"ash", "Ashburn, VA"},
	"HIL1": {"hil", "Hillsboro, OR"},
	"SIN1": {"sin", "Singapore"},
}

// hetznerServer is one row of a cloud server page.
type hetznerServer struct {
	Type     string   `json:"type"` // the Cloud API's name, such as cx23
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Cores    string   `json:"cores"`
	CPU      string   `json:"cpu"`
	Memory   string   `json:"memory"` // GB
	Disk     string   `json:"disk"`   // GB
	DiskType string   `json:"diskType"`
	Products []string `json:"products"` // the server's product id, then the primary IPv4's
}

type hetznerProduct struct {
	ProductID string            `json:"productId"`
	Locations []hetznerLocation `json:"locations"`
}

type hetznerLocation struct {
	CountryCode string                       `json:"countryCode"`
	Active      bool                         `json:"active"`
	Datacenter  string                       `json:"datacenter"`
	Prices      map[string]map[string]string `json:"prices"` // hourly or monthly -> currency -> price
}

type hetznerSource struct {
	Kind      string                    `json:"kind"`
	FetchedAt string                    `json:"fetchedAt"`
	Servers   []hetznerServer           `json:"servers"`
	Products  map[string]hetznerProduct `json:"products"`
}

var (
	hetznerRow     = regexp.MustCompile(`class="cloud-matrix-product[ "]`)
	hetznerCell    = regexp.MustCompile(`<div class="cell ([a-z-]+)-cell[^"]*">`)
	hetznerType    = regexp.MustCompile(`create/server\?type=([a-z0-9-]+)`)
	hetznerKey     = regexp.MustCompile(`product-key="(CLOUD_\d+(?:\+CLOUD_\d+)*)"`)
	hetznerSVG     = regexp.MustCompile(`(?s)<svg.*?</svg>`)
	hetznerTag     = regexp.MustCompile(`<[^>]+>`)
	hetznerUnavail = regexp.MustCompile(`(?i)\s*not available.*$`)
)

// parseHetznerPage reads the servers from one cloud server page. A row that can't be read is an error, not a
// server left out, so a change to the page stops the update rather than deleting the servers from the bundle.
func parseHetznerPage(page, category string) ([]hetznerServer, error) {
	var servers []hetznerServer
	seen := map[string]bool{}
	starts := hetznerRow.FindAllStringIndex(page, -1)
	for i, start := range starts {
		end := len(page)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		row := page[start[0]:end]
		typ := hetznerType.FindStringSubmatch(row)
		if typ == nil {
			continue // not a server, such as a page's promotional row
		}
		if seen[typ[1]] {
			continue // the page lists its servers once per screen size
		}
		seen[typ[1]] = true
		key := hetznerKey.FindStringSubmatch(row)
		cells := hetznerCells(row)
		name := hetznerUnavail.ReplaceAllString(cells["name"], "")
		cores, cpu, _ := strings.Cut(cells["cpu"], " ")
		disk := strings.Fields(cells["drive"])
		if key == nil || name == "" || cpu == "" || len(disk) < 3 || !strings.HasSuffix(cells["ram"], " GB") {
			return nil, fmt.Errorf("can't read the row for %s: %q", typ[1], cells)
		}
		if _, err := strconv.Atoi(cores); err != nil {
			return nil, fmt.Errorf("can't read the row for %s: %q", typ[1], cells)
		}
		servers = append(servers, hetznerServer{
			Type: typ[1], Name: name, Category: category,
			Cores: cores, CPU: cpu,
			Memory: strings.TrimSuffix(cells["ram"], " GB"), Disk: disk[0], DiskType: disk[2],
			Products: strings.Split(key[1], "+"),
		})
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no servers found")
	}
	return servers, nil
}

// hetznerCells gives the text of each cell in a row, by the name in its class.
func hetznerCells(row string) map[string]string {
	cells := map[string]string{}
	locs := hetznerCell.FindAllStringSubmatchIndex(row, -1)
	for i, loc := range locs {
		end := len(row)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		text := hetznerSVG.ReplaceAllString(row[loc[1]:end], " ")
		text = hetznerTag.ReplaceAllString(text, " ")
		cells[row[loc[2]:loc[3]]] = strings.Join(strings.Fields(html.UnescapeString(text)), " ")
	}
	return cells
}

// fetchHetzner reads the cloud server pages and the price of every product on them.
func fetchHetzner(ctx context.Context, client *http.Client, siteURL, priceURL string) (*hetznerSource, error) {
	fetched := time.Now().UTC()
	src := &hetznerSource{Kind: hetznerSourceKind, FetchedAt: fetched.Format(time.RFC3339), Products: map[string]hetznerProduct{}}
	seen := map[string]bool{}
	for _, p := range hetznerPages {
		page, err := getText(ctx, client, siteURL+p.path)
		if err != nil {
			return nil, fmt.Errorf("Hetzner %s: %w", p.path, err)
		}
		servers, err := parseHetznerPage(page, p.category)
		if err != nil {
			return nil, fmt.Errorf("Hetzner %s: %w", p.path, err)
		}
		for _, s := range servers {
			if !seen[s.Type] {
				seen[s.Type] = true
				src.Servers = append(src.Servers, s)
			}
		}
	}
	var ids []string
	for _, s := range src.Servers {
		for _, id := range s.Products {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, 8)
	)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var p hetznerProduct
			err := getJSON(ctx, client, priceURL+"/products/"+id, &p)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("Hetzner price API (%s): %w", id, err)
			}
			src.Products[id] = p
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return src, nil
}

// getText GETs a URL as text, retrying throttling and server errors.
func getText(ctx context.Context, client *http.Client, u string) (string, error) {
	var last error
	for attempt := range 6 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "other-instances")
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
			return "", fmt.Errorf("%s", resp.Status)
		}
		return string(data), nil
	}
	return "", last
}

// hetznerUSD reads a price in USD; ok is false if the location doesn't have that price.
func hetznerUSD(prices map[string]map[string]string, period string) (float64, bool, error) {
	s, ok := prices[period]["USD"]
	if !ok {
		return 0, false, nil
	}
	p, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, fmt.Errorf("bad %s price %q", period, s)
	}
	return p, true, nil
}

// hetznerMonthlyTerm is what a server costs at most per month: Hetzner bills it hourly up to this cap.
var hetznerMonthlyTerm = Term{"1mo", "Monthly Cap", "Postpaid"}

// readHetzner turns a fetched Hetzner file into a snapshot.
func readHetzner(r io.Reader) (*snapshot, error) {
	var src hetznerSource
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("decoding Hetzner file: %w", err)
	}
	if src.Kind != hetznerSourceKind {
		return nil, fmt.Errorf("not a Hetzner file (kind %q)", src.Kind)
	}
	ts, err := parseTS(src.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("fetchedAt: %w", err)
	}
	s := &snapshot{Offer: hetznerOffer, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}}
	for _, server := range src.Servers {
		s.Products++
		if len(server.Products) == 0 {
			return nil, fmt.Errorf("%s has no product id", server.Type)
		}
		// The primary IPv4 is priced the same everywhere, as one location named ALL.
		var ipHourly, ipMonthly float64
		for _, id := range server.Products[1:] {
			for _, loc := range src.Products[id].Locations {
				if loc.Datacenter != "ALL" {
					continue
				}
				h, _, err := hetznerUSD(loc.Prices, "hourly")
				if err != nil {
					return nil, fmt.Errorf("%s: %w", id, err)
				}
				m, _, err := hetznerUSD(loc.Prices, "monthly")
				if err != nil {
					return nil, fmt.Errorf("%s: %w", id, err)
				}
				ipHourly, ipMonthly = ipHourly+h, ipMonthly+m
			}
		}
		si := newSnapInstance()
		arch := "x86_64"
		if strings.Contains(server.CPU, "Ampere") {
			arch = "arm64"
		}
		for _, loc := range src.Products[server.Products[0]].Locations {
			if !loc.Active {
				continue
			}
			known, ok := hetznerLocations[loc.Datacenter]
			if !ok {
				s.Skipped++
				continue
			}
			hourly, hasHourly, err := hetznerUSD(loc.Prices, "hourly")
			if err != nil {
				return nil, fmt.Errorf("%s in %s: %w", server.Type, loc.Datacenter, err)
			}
			monthly, hasMonthly, err := hetznerUSD(loc.Prices, "monthly")
			if err != nil {
				return nil, fmt.Errorf("%s in %s: %w", server.Type, loc.Datacenter, err)
			}
			if !hasHourly {
				s.Skipped++
				continue
			}
			region := known.code
			s.Regions[region] = map[string]string{"regionCode": region, "locationType": "Hetzner Location", "name": known.name, "country": loc.CountryCode}
			variants := []struct {
				name            string
				hourly, monthly float64
			}{
				{hetznerIPv6Only, hourly, monthly},
				{hetznerWithIPv4, roundPrice(hourly + ipHourly), roundPrice(monthly + ipMonthly)},
			}
			for _, v := range variants {
				variant := Variant{v.name}
				s.setOD(si, odKey{Region: region, Variant: variant}, v.hourly)
				if hasMonthly {
					s.setRS(si, rsKey{Region: region, Variant: variant, Term: hetznerMonthlyTerm}, 0, v.monthly)
				}
			}
			si.Attrs[region] = map[string]string{
				"name": server.Name, "category": server.Category, "cores": server.Cores, "cpu": server.CPU, "architecture": arch,
				"memoryInGb": server.Memory, "diskInGb": server.Disk, "diskType": server.DiskType, "productFamily": "Cloud Server",
			}
		}
		if len(si.OD) == 0 {
			s.Unpriced++
			continue
		}
		s.Instances[server.Type] = si
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("Hetzner file has no prices")
	}
	return s, nil
}

// --- Server Auction ---

// hetznerAuctionSource is the Server Auction's listings, each kept as it came apart from the fields that change
// on their own as the auction runs.
type hetznerAuctionSource struct {
	Kind      string           `json:"kind"`
	FetchedAt string           `json:"fetchedAt"`
	Servers   []map[string]any `json:"servers"`
}

// fetchHetznerAuction reads the Server Auction's current listings.
func fetchHetznerAuction(ctx context.Context, client *http.Client, auctionURL string) (*hetznerAuctionSource, error) {
	fetched := time.Now().UTC()
	var page struct {
		Server []map[string]any `json:"server"`
	}
	if err := getJSON(ctx, client, auctionURL, &page); err != nil {
		return nil, fmt.Errorf("Hetzner Server Auction: %w", err)
	}
	if len(page.Server) == 0 {
		return nil, fmt.Errorf("Hetzner Server Auction lists no servers")
	}
	return &hetznerAuctionSource{Kind: hetznerAuctionSourceKind, FetchedAt: fetched.Format(time.RFC3339), Servers: page.Server}, nil
}

// hetznerAuctionPrice reads Prices.<period>.USD or IPPrices.<period>.USD from a listing.
func hetznerAuctionPrice(server map[string]any, field, period string) (float64, bool) {
	prices, _ := server[field].(map[string]any)
	byCurrency, _ := prices[period].(map[string]any)
	n, ok := byCurrency["USD"].(json.Number)
	if !ok {
		return 0, false
	}
	p, err := n.Float64()
	return p, err == nil
}

// readHetznerAuction turns a fetched Server Auction file into a snapshot.
func readHetznerAuction(r io.Reader) (*snapshot, error) {
	var src hetznerAuctionSource
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("decoding Hetzner auction file: %w", err)
	}
	if src.Kind != hetznerAuctionSourceKind {
		return nil, fmt.Errorf("not a Hetzner auction file (kind %q)", src.Kind)
	}
	ts, err := parseTS(src.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("fetchedAt: %w", err)
	}
	s := &snapshot{Offer: hetznerAuctionOffer, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}}
	for _, server := range src.Servers {
		s.Products++
		id, ok := server["Id"].(json.Number)
		hourly, hasHourly := hetznerAuctionPrice(server, "Prices", "hourly")
		monthly, hasMonthly := hetznerAuctionPrice(server, "Prices", "monthly")
		details, _ := server["Details"].(map[string]any)
		dc, _ := details["Datacenter"].(map[string]any)
		dcName, _ := dc["Name"].(string) // such as FSN1-DC4
		if !ok || !hasHourly || !hasMonthly || dcName == "" {
			s.Skipped++
			continue
		}
		site, _, _ := strings.Cut(dcName, "-")
		region := strings.ToLower(site)
		if known, ok := hetznerLocations[site]; ok {
			region = known.code
			s.Regions[region] = map[string]string{"regionCode": region, "locationType": "Hetzner Location", "name": known.name}
		} else {
			s.Regions[region] = map[string]string{"regionCode": region, "locationType": "Hetzner Location"}
		}
		attrs := map[string]string{"datacenter": dcName, "productFamily": "Dedicated Server"}
		flatten("", server["Hardware"], attrs)
		for k, v := range details {
			if k != "Datacenter" {
				flatten(k, v, attrs)
			}
		}
		prices, _ := server["Prices"].(map[string]any)
		if fixed, ok := prices["fixed"].(bool); ok {
			attrs["fixedPrice"] = strconv.FormatBool(fixed)
		}
		if setup, ok := hetznerAuctionPrice(server, "Prices", "setup"); ok {
			attrs["setupFee"] = strconv.FormatFloat(setup, 'f', -1, 64)
		}

		si := newSnapInstance()
		si.Attrs[region] = attrs
		ipHourly, _ := hetznerAuctionPrice(server, "IPPrices", "hourly")
		ipMonthly, _ := hetznerAuctionPrice(server, "IPPrices", "monthly")
		s.setOD(si, odKey{Region: region, Variant: Variant{hetznerIPv6Only}}, hourly)
		s.setRS(si, rsKey{Region: region, Variant: Variant{hetznerIPv6Only}, Term: hetznerMonthlyTerm}, 0, monthly)
		s.setOD(si, odKey{Region: region, Variant: Variant{hetznerWithIPv4}}, roundPrice(hourly+ipHourly))
		s.setRS(si, rsKey{Region: region, Variant: Variant{hetznerWithIPv4}, Term: hetznerMonthlyTerm}, 0, roundPrice(monthly+ipMonthly))
		s.Instances[id.String()] = si
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("Hetzner auction file has no prices")
	}
	return s, nil
}
