package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Alibaba Cloud publishes the prices behind its ECS pricing page as a versioned package on its CDN:
// https://g.alicdn.com/aliyun/ecs-price-info-intl/2.0.<n>/... Every version stays published, so a bundle can
// be seeded with the full history (2.0.0, from May 2018, onwards), the way AWS bundles are. Each version
// has up to three files:
//
//   - price/download/instancePrice.json: pay-as-you-go and subscription prices, keyed
//     "RegionId::InstanceType::NetworkType::OSType::IoOptimized", with a publicationDate. Every version has it.
//   - price/js_price/filter_price/reservedInstancePrice.js: reserved instance prices. From 2.0.20.
//   - price/js_price/filter_price/instanceTypeDefinition.js: DescribeInstanceTypes for every instance
//     type, used for attributes. From 2.0.210.
//
// `fetch alibaba` downloads one version's files into a directory, which compress and append read.

const alibabaBase = "https://g.alicdn.com/aliyun/ecs-price-info-intl"

// alibabaVersionPrefix is the part of the version every published version so far shares.
const alibabaVersionPrefix = "2.0."

const (
	aliPriceFile    = "instancePrice.json"
	aliReservedFile = "reservedInstancePrice.js"
	aliDefsFile     = "instanceTypeDefinition.js"
	aliSourceFile   = "source.json"
)

var aliPaths = map[string]string{
	aliPriceFile:    "price/download/instancePrice.json",
	aliReservedFile: "price/js_price/filter_price/reservedInstancePrice.js",
	aliDefsFile:     "price/js_price/filter_price/instanceTypeDefinition.js",
}

// aliSourceKind marks a fetched Alibaba directory's source.json.
const aliSourceKind = "alibaba-ecs-price-info"

var aliOS = map[string]string{"linux": "Linux", "windows": "Windows"}
var aliNetwork = map[string]string{"vpc": "VPC", "classic": "Classic"}
var aliUnits = map[string]string{"hours": "hr", "days": "d", "weeks": "wk", "months": "mo", "years": "yr",
	"Hour": "hr", "Day": "d", "Week": "wk", "Month": "mo", "Year": "yr"}

// aliRegionNames are the English names of Alibaba Cloud regions. Regions not listed here just have no name.
var aliRegionNames = map[string]string{
	"ap-northeast-1": "Japan (Tokyo)",
	"ap-northeast-2": "South Korea (Seoul)",
	"ap-south-1":     "India (Mumbai)",
	"ap-southeast-1": "Singapore",
	"ap-southeast-2": "Australia (Sydney)",
	"ap-southeast-3": "Malaysia (Kuala Lumpur)",
	"ap-southeast-5": "Indonesia (Jakarta)",
	"ap-southeast-6": "Philippines (Manila)",
	"ap-southeast-7": "Thailand (Bangkok)",
	"cn-beijing":     "China (Beijing)",
	"cn-chengdu":     "China (Chengdu)",
	"cn-fuzhou":      "China (Fuzhou)",
	"cn-guangzhou":   "China (Guangzhou)",
	"cn-hangzhou":    "China (Hangzhou)",
	"cn-heyuan":      "China (Heyuan)",
	"cn-hongkong":    "China (Hong Kong)",
	"cn-huhehaote":   "China (Hohhot)",
	"cn-nanjing":     "China (Nanjing)",
	"cn-qingdao":     "China (Qingdao)",
	"cn-shanghai":    "China (Shanghai)",
	"cn-shenzhen":    "China (Shenzhen)",
	"cn-wulanchabu":  "China (Ulanqab)",
	"cn-zhangjiakou": "China (Zhangjiakou)",
	"eu-central-1":   "Germany (Frankfurt)",
	"eu-west-1":      "UK (London)",
	"me-central-1":   "Saudi Arabia (Riyadh)",
	"me-east-1":      "UAE (Dubai)",
	"na-south-1":     "Mexico",
	"us-east-1":      "US (Virginia)",
	"us-west-1":      "US (Silicon Valley)",
}

type aliSource struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

type aliPriceEntry struct {
	Price  string `json:"price"`
	Period string `json:"period"`
}

type aliPrices struct {
	Currency        string                                `json:"currency"`
	Version         string                                `json:"version"`
	PublicationDate string                                `json:"publicationDate"`
	PricingInfo     map[string]map[string][]aliPriceEntry `json:"pricingInfo"`
}

type aliRIEntry struct {
	ReservedHourPrice string `json:"reservedHourPrice"`
	PrepayPrice       string `json:"prepayPrice"`
	PriceUnit         string `json:"priceUnit"`
	Period            string `json:"period"`
}

// region -> instance type -> os -> payment option -> unit -> prices
type aliReserved map[string]map[string]map[string]map[string]map[string][]aliRIEntry

// isAlibabaSource reports whether path is a directory of Alibaba price files.
func isAlibabaSource(path string) bool {
	_, err := os.Stat(filepath.Join(path, aliPriceFile))
	return err == nil
}

// readAlibaba turns a directory of one version's Alibaba price files into a snapshot.
func readAlibaba(dir string) (*snapshot, error) {
	var prices aliPrices
	if err := readJSONFile(filepath.Join(dir, aliPriceFile), &prices); err != nil {
		return nil, err
	}
	if prices.Currency != "USD" {
		return nil, fmt.Errorf("%s: prices are in %q, not USD", aliPriceFile, prices.Currency)
	}
	ts, err := parseTS(prices.PublicationDate)
	if err != nil {
		return nil, fmt.Errorf("%s: publicationDate: %w", aliPriceFile, err)
	}
	// The files' own version field doesn't always match the path they are published under (2.0.0 says
	// 1.2.3), so the version fetch recorded is preferred.
	version := prices.Version
	var src aliSource
	if err := readJSONFile(filepath.Join(dir, aliSourceFile), &src); err == nil && src.Version != "" {
		version = src.Version
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	s := &snapshot{Offer: alibabaOffer, TS: ts, Version: version, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}}
	inst := func(region, itype string) *snapInstance {
		if s.Regions[region] == nil {
			s.Regions[region] = aliRegionAttrs(region)
		}
		si := s.Instances[itype]
		if si == nil {
			si = newSnapInstance()
			s.Instances[itype] = si
		}
		return si
	}

	for key, units := range prices.PricingInfo {
		parts := strings.Split(key, "::")
		if len(parts) != 5 {
			return nil, fmt.Errorf("%s: key %q is not RegionId::InstanceType::NetworkType::OSType::IoOptimized", aliPriceFile, key)
		}
		region, itype, network, osType, io := parts[0], parts[1], parts[2], parts[3], parts[4]
		s.Products++
		// Every version so far prices only I/O optimized instances; anything else would need its own variant key.
		if io != "optimized" {
			s.Skipped++
			continue
		}
		si := inst(region, itype)
		variant := Variant{named(aliOS, osType), named(aliNetwork, network)}
		for unit, entries := range units {
			for _, e := range entries {
				if e.Price == "" {
					continue
				}
				p, err := strconv.ParseFloat(e.Price, 64)
				if err != nil {
					return nil, fmt.Errorf("%s: %s %s %s: bad price %q", aliPriceFile, key, unit, e.Period, e.Price)
				}
				if unit == "hours" && e.Period == "1" {
					s.setOD(si, odKey{Region: region, Variant: variant}, p)
					continue
				}
				lease, err := aliLease(unit, e.Period)
				if err != nil {
					return nil, fmt.Errorf("%s: %s: %w", aliPriceFile, key, err)
				}
				// A subscription is paid in full when it is bought, like an AWS All Upfront reservation.
				s.setRS(si, rsKey{Region: region, Variant: variant, Term: Term{lease, "Subscription", "All Upfront"}}, 0, p)
			}
		}
	}

	var ri aliReserved
	switch err := readJSVar(filepath.Join(dir, aliReservedFile), &ri); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	}
	for region, types := range ri {
		for itype, oses := range types {
			for osType, options := range oses {
				variant := Variant{named(aliOS, osType), aliNetwork["vpc"]}
				for option, units := range options {
					for unit, entries := range units {
						for _, e := range entries {
							if e.ReservedHourPrice == "" && e.PrepayPrice == "" {
								continue
							}
							lease, err := aliLease(cmpOr(e.PriceUnit, unit), e.Period)
							if err != nil {
								return nil, fmt.Errorf("%s: %s %s: %w", aliReservedFile, region, itype, err)
							}
							k := rsKey{Region: region, Variant: variant, Term: Term{lease, "Reserved Instance", strings.ReplaceAll(option, "_", " ")}}
							si := inst(region, itype)
							if e.PrepayPrice == "PriceNotFound" {
								continue // Alibaba had no price for it: not offered
							}
							hourly, err1 := parsePrice(e.ReservedHourPrice)
							upfront, err2 := parsePrice(e.PrepayPrice)
							// Other words in place of a price are errors Alibaba hit while it built the file
							// ("Throttling", "InvalidVersion"), so the price is unknown, not gone. Many versions
							// have a few hundred; the bundle keeps the price it had.
							if err1 != nil || err2 != nil {
								if _, priced := si.RS[k]; !priced {
									si.KeepRS[k] = true
								}
								s.Unknown++
								continue
							}
							delete(si.KeepRS, k)
							s.setRS(si, k, hourly, upfront)
						}
					}
				}
			}
		}
	}

	var defs []map[string]any
	switch err := readJSVar(filepath.Join(dir, aliDefsFile), &defs); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	}
	for _, d := range defs {
		itype, _ := d["InstanceTypeId"].(string)
		si := s.Instances[itype]
		if si == nil {
			continue // described but not priced
		}
		attrs := map[string]string{}
		for k, v := range d {
			if k != "InstanceTypeId" {
				flatten(k, v, attrs)
			}
		}
		// The definitions are the same in every region, so they are stored as one set.
		si.Attrs[""] = attrs
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("%s has no prices", dir)
	}
	return s, nil
}

func aliRegionAttrs(region string) map[string]string {
	attrs := map[string]string{"regionCode": region, "locationType": "Alibaba Cloud Region"}
	if name, ok := aliRegionNames[region]; ok {
		attrs["name"] = name
	}
	return attrs
}

// aliLease names a subscription or reservation length the way AWS bundles do: "1mo", "3yr".
func aliLease(unit, period string) (string, error) {
	u, ok := aliUnits[unit]
	n, err := strconv.Atoi(period)
	if !ok || err != nil || n < 1 {
		return "", fmt.Errorf("unknown term %q %q", period, unit)
	}
	return strconv.Itoa(n) + u, nil
}

func named(names map[string]string, v string) string {
	if n, ok := names[v]; ok {
		return n
	}
	return v
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func parsePrice(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	p, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad price %q", s)
	}
	return p, nil
}

func readJSONFile(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// readJSVar reads a script of the form `var name = <JSON>;`, keeping numbers as their source text.
func readJSVar(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	i := bytes.IndexByte(data, '=')
	if i < 0 || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("var ")) {
		return fmt.Errorf("%s: not a `var name = ...` script", filepath.Base(path))
	}
	body := bytes.TrimRight(bytes.TrimSpace(data[i+1:]), ";")
	if err := decodeNumbers(body, dst); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// fetchAlibaba downloads one version's files into dir. The price file must exist; the others are left out
// if the version doesn't have them.
func fetchAlibaba(ctx context.Context, client *http.Client, base, version, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{aliPriceFile, aliReservedFile, aliDefsFile} {
		url := fmt.Sprintf("%s/%s/%s", base, version, aliPaths[name])
		err := download(ctx, client, url, filepath.Join(dir, name))
		if errors.Is(err, errNotFound) && name != aliPriceFile {
			continue
		}
		if err != nil {
			return err
		}
	}
	src, _ := json.Marshal(aliSource{Kind: aliSourceKind, Version: version})
	return os.WriteFile(filepath.Join(dir, aliSourceFile), append(src, '\n'), 0o644)
}

var errNotFound = errors.New("not found")

func download(ctx context.Context, client *http.Client, url, path string) error {
	var last error
	for attempt := range 5 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 5 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return fmt.Errorf("%s: %w", url, errNotFound)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			last = fmt.Errorf("%s: %s", url, resp.Status)
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return last
			}
			continue
		}
		f, err := os.Create(path)
		if err != nil {
			resp.Body.Close()
			return err
		}
		_, err = io.Copy(f, resp.Body)
		resp.Body.Close()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			last = fmt.Errorf("%s: %w", url, err)
			continue
		}
		return nil
	}
	return last
}

// alibabaVersions lists the published versions after `after` (all of them if it is ""), oldest first.
// Versions are numbered 2.0.<n> with a few numbers skipped (at most 3 in a row so far), so it probes upward
// until `window` numbers in a row are missing.
func alibabaVersions(ctx context.Context, client *http.Client, base, after string, window int) ([]string, error) {
	next := 0
	if after != "" {
		n, err := alibabaPatch(after)
		if err != nil {
			return nil, err
		}
		next = n + 1
	}
	const batch = 16
	var found []string
	misses := 0
	for misses < window {
		hits := make([]bool, batch)
		errs := make([]error, batch)
		var wg sync.WaitGroup
		for i := range batch {
			wg.Add(1)
			go func() {
				defer wg.Done()
				url := fmt.Sprintf("%s/%s%d/%s", base, alibabaVersionPrefix, next+i, aliPaths[aliPriceFile])
				hits[i], errs[i] = exists(ctx, client, url)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		for i, hit := range hits {
			if misses >= window {
				break
			}
			if hit {
				found = append(found, fmt.Sprintf("%s%d", alibabaVersionPrefix, next+i))
				misses = 0
			} else {
				misses++
			}
		}
		next += batch
	}
	return found, nil
}

// alibabaPatch reads n from a version 2.0.n.
func alibabaPatch(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(v, alibabaVersionPrefix))
	if !strings.HasPrefix(v, alibabaVersionPrefix) || err != nil || n < 0 {
		return 0, fmt.Errorf("version %q is not %s<n>", v, alibabaVersionPrefix)
	}
	return n, nil
}

func exists(ctx context.Context, client *http.Client, url string) (bool, error) {
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		if err != nil {
			return false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			return true, nil
		// The CDN answers 404 for versions that were never published.
		case resp.StatusCode == http.StatusNotFound:
			return false, nil
		default:
			last = fmt.Errorf("%s: %s", url, resp.Status)
		}
	}
	return false, last
}
