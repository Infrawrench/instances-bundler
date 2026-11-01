package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// spotProducts maps the product descriptions in EC2's spot price history to the feed's operating systems.
// Accounts made before VPCs were the default see the "(Amazon VPC)" spellings.
var spotProducts = map[string]string{
	"Linux/UNIX": "Linux", "Linux/UNIX (Amazon VPC)": "Linux",
	"Windows": "Windows", "Windows (Amazon VPC)": "Windows",
}

// spotAggregates turn the prices in a region's availability zones into one price for the region, as the feed has.
var spotAggregates = map[string]func([]float64) float64{
	"min": slices.Min[[]float64],
	"max": slices.Max[[]float64],
	"mean": func(p []float64) float64 {
		sum := 0.0
		for _, x := range p {
			sum += x
		}
		return round4(sum / float64(len(p)))
	},
	"median": func(p []float64) float64 {
		s := slices.Sorted(slices.Values(p))
		if len(s)%2 == 1 {
			return s[len(s)/2]
		}
		return round4((s[len(s)/2-1] + s[len(s)/2]) / 2)
	},
}

var spotAggregateNames = []string{"min", "max", "mean", "median"}

// The feed's prices have at most 4 decimal places.
func round4(p float64) float64 { return math.Round(p*1e4) / 1e4 }

// zoneSuffix is the letter that ends an availability zone's name: us-east-1a is in us-east-1, and
// us-east-1-atl-1a in the us-east-1-atl-1 Local Zone group, which is how the feed names Local Zones.
var zoneSuffix = regexp.MustCompile(`[a-z]+$`)

type spotHistoryRecord struct {
	AvailabilityZone   string
	InstanceType       string
	ProductDescription string
	SpotPrice          string
	Timestamp          string
}

// spotEvent is a change in a region's price for one instance type and operating system.
type spotEvent struct {
	TS    int64
	Name  string
	Key   odKey
	Price float64
}

type spotPriceKey struct {
	Name string
	Key  odKey
}

// spotHistory is EC2's spot price history reduced to region price changes.
type spotHistory struct {
	Events  []spotEvent
	Final   map[string]map[spotPriceKey]float64 // aggregate -> price at the end, to compare with the feed
	Records int
}

// readSpotHistory reads describe-spot-price-history pages (gzipped JSON, as seed-spot.sh saves them) from
// dir/<region>/*.json.gz. Records from before start count as the price at start; records from end on are skipped.
func readSpotHistory(dir string, start, end int64, aggregate string) (*spotHistory, error) {
	agg, ok := spotAggregates[aggregate]
	if !ok {
		return nil, fmt.Errorf("unknown aggregate %q; use one of %s", aggregate, strings.Join(spotAggregateNames, ", "))
	}
	regionDirs, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return nil, err
	}
	h := &spotHistory{Final: map[string]map[spotPriceKey]float64{}}
	for _, name := range spotAggregateNames {
		h.Final[name] = map[spotPriceKey]float64{}
	}
	for _, rd := range regionDirs {
		pages, err := filepath.Glob(filepath.Join(rd, "*.json.gz"))
		if err != nil {
			return nil, err
		}
		if len(pages) == 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(rd, "done")); err != nil {
			return nil, fmt.Errorf("%s was not fetched completely; run seed-spot.sh again to finish it", rd)
		}
		if err := h.addRegion(pages, start, end, agg); err != nil {
			return nil, fmt.Errorf("%s: %w", rd, err)
		}
	}
	sort.SliceStable(h.Events, func(i, j int) bool { return h.Events[i].TS < h.Events[j].TS })
	return h, nil
}

type azPrice struct {
	TS    int64
	AZ    string
	Price float64
}

func (h *spotHistory) addRegion(pages []string, start, end int64, agg func([]float64) float64) error {
	series := map[spotPriceKey][]azPrice{}
	strs := map[string]string{}
	intern := func(s string) string {
		if v, ok := strs[s]; ok {
			return v
		}
		strs[s] = s
		return s
	}
	for _, path := range pages {
		var page struct{ SpotPriceHistory []spotHistoryRecord }
		if err := readGzipJSON(path, &page); err != nil {
			return err
		}
		for _, r := range page.SpotPriceHistory {
			os, ok := spotProducts[r.ProductDescription]
			if !ok {
				continue
			}
			ts, err := parseTS(r.Timestamp)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if ts >= end {
				continue
			}
			p, err := strconv.ParseFloat(r.SpotPrice, 64)
			if err != nil {
				return fmt.Errorf("%s: bad price %q", path, r.SpotPrice)
			}
			h.Records++
			k := spotPriceKey{intern(r.InstanceType), odKey{intern(zoneSuffix.ReplaceAllString(r.AvailabilityZone, "")), Variant{os}}}
			series[k] = append(series[k], azPrice{ts, intern(r.AvailabilityZone), p})
		}
	}
	for k, recs := range series {
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].TS < recs[j].TS })
		cur := map[string]float64{}
		last, has := 0.0, false
		for i := 0; i < len(recs); {
			// Apply every record up to the next timestamp, counting records before start as at start.
			ts := max(recs[i].TS, start)
			for ; i < len(recs) && max(recs[i].TS, start) == ts; i++ {
				cur[recs[i].AZ] = recs[i].Price
			}
			p := agg(slices.Collect(maps.Values(cur)))
			if !has || p != last {
				h.Events = append(h.Events, spotEvent{ts, k.Name, k.Key, p})
				last, has = p, true
			}
		}
		prices := slices.Collect(maps.Values(cur))
		for name, f := range spotAggregates {
			h.Final[name][k] = f(prices)
		}
	}
	return nil
}

func readGzipJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := json.NewDecoder(zr).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// appendSpotHistory adds the history to the bundle as one snapshot per interval in which prices changed,
// the first at start. Each row is dated by when its price changed, not by its snapshot.
func appendSpotHistory(b *Bundle, h *spotHistory, start, end, interval int64) (AppendStats, int, error) {
	var total AppendStats
	state := map[string]*snapInstance{}
	n, i := 0, 0
	for t := start; i < len(h.Events); t += interval {
		boundary := min(t, end-1)
		applied := false
		for ; i < len(h.Events) && h.Events[i].TS <= boundary; i++ {
			e := h.Events[i]
			si := state[e.Name]
			if si == nil {
				si = &snapInstance{Attrs: map[string]map[string]string{}, OD: map[odKey]odPrice{}, RS: map[rsKey]rsPrice{}}
				state[e.Name] = si
			}
			si.OD[e.Key] = odPrice{Eff: e.TS, Price: e.Price}
			applied = true
		}
		if !applied {
			continue
		}
		// Append only reads the snapshot, so it can share the state's maps.
		s := &snapshot{Offer: spotOffer, TS: boundary, Instances: state, Regions: map[string]map[string]string{}}
		st, err := b.Append(s)
		if err != nil {
			return total, n, err
		}
		n++
		total.Created += st.Created
		total.ODRows += st.ODRows
	}
	return total, n, nil
}

// compareSpot reports how well each aggregate of the history's final prices matches the feed's prices.
func compareSpot(h *spotHistory, feed *snapshot, chosen string) string {
	var out strings.Builder
	matches := map[string]int{}
	both := 0
	missing := map[string]int{}
	for name, si := range feed.Instances {
		for k, p := range si.OD {
			pk := spotPriceKey{name, k}
			if _, ok := h.Final["min"][pk]; !ok {
				missing[k.Region]++
				continue
			}
			both++
			for _, a := range spotAggregateNames {
				if round4(h.Final[a][pk]) == round4(p.Price) {
					matches[a]++
				}
			}
		}
	}
	fmt.Fprintf(&out, "compared the history's latest prices with the feed's, for %d prices in both:\n", both)
	best := chosen
	for _, a := range spotAggregateNames {
		mark := ""
		if a == chosen {
			mark = " (used)"
		}
		pct := 0.0
		if both > 0 {
			pct = 100 * float64(matches[a]) / float64(both)
		}
		fmt.Fprintf(&out, "  %-6s %6.2f%% match%s\n", a, pct, mark)
		if matches[a] > matches[best] {
			best = a
		}
	}
	if best != chosen {
		fmt.Fprintf(&out, "warning: -aggregate %s matches the feed better than %s\n", best, chosen)
	}
	if len(missing) > 0 {
		var parts []string
		for _, r := range slices.Sorted(maps.Keys(missing)) {
			parts = append(parts, fmt.Sprintf("%s (%d)", r, missing[r]))
		}
		fmt.Fprintf(&out, "feed prices with no history, by region: %s\n", strings.Join(parts, ", "))
	}
	return out.String()
}
