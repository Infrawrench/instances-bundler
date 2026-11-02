package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// spotJSON builds a spot price feed; prices maps region -> instance type -> [linux, mswin] price strings.
func spotJSON(t *testing.T, prices map[string]map[string][2]string) []byte {
	t.Helper()
	type col struct {
		Name   string            `json:"name"`
		Prices map[string]string `json:"prices"`
	}
	type size struct {
		Size         string `json:"size"`
		ValueColumns []col  `json:"valueColumns"`
	}
	var regions []map[string]any
	for region, types := range prices {
		var sizes []size
		for name, p := range types {
			sizes = append(sizes, size{name, []col{{"linux", map[string]string{"USD": p[0]}}, {"mswin", map[string]string{"USD": p[1]}}}})
		}
		regions = append(regions, map[string]any{
			"region":    region,
			"footnotes": map[string]string{"*": "notAvailableForCCorCGPU"},
			// The feed repeats some types within a region; repeating every one checks they are merged.
			"instanceTypes": []map[string]any{{"type": "generalCurrentGen", "sizes": sizes}, {"type": "gpuCurrentGen", "sizes": sizes}},
		})
	}
	data, err := json.Marshal(map[string]any{"vers": 0.01, "config": map[string]any{
		"rate": "perhr", "valueColumns": []string{"linux", "mswin"}, "currencies": []string{"USD"}, "regions": regions,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func spotSnapshot(t *testing.T, ts string, prices map[string]map[string][2]string) *snapshot {
	t.Helper()
	data := spotJSON(t, prices)
	if !isSpotFeed(data) {
		t.Fatal("isSpotFeed should recognise a spot price feed")
	}
	s, err := readSpot(bytes.NewReader(data), mustTS(t, ts))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSpot(t *testing.T) {
	nov := spotSnapshot(t, "2026-11-01T09:00:00Z", map[string]map[string][2]string{
		"us-east-1": {"m5.large": {"0.0405", "N/A*"}, "t4g.nano": {"5.0E-4", "N/A*"}, "p5.48xlarge": {"N/A*", "N/A*"}},
		"eu-west-1": {"m5.large": {"0.0495", "0.1179"}},
	})
	if nov.Offer != spotOffer || nov.Collisions != 0 {
		t.Errorf("offer %q, %d collisions", nov.Offer, nov.Collisions)
	}
	if _, ok := nov.Instances["p5.48xlarge"]; ok {
		t.Error("an instance type with no spot prices should be left out")
	}
	if got := nov.Instances["t4g.nano"].OD[odKey{"us-east-1", Variant{"Linux"}}].Price; got != 0.0005 {
		t.Errorf("t4g.nano price %v, want 0.0005", got)
	}
	b := appendOK(t, &Bundle{Instances: map[string]*Instance{}}, nov, AppendStats{Created: 2, ODRows: 4, RegionChanges: 4})
	if b.Offer != spotOffer {
		t.Errorf("offer %q, want %s", b.Offer, spotOffer)
	}

	// m5.large gets cheaper in us-east-1, Windows stops in eu-west-1, t4g.nano goes away.
	later := spotSnapshot(t, "2026-11-01T09:15:00Z", map[string]map[string][2]string{
		"us-east-1": {"m5.large": {"0.0401", "N/A*"}},
		"eu-west-1": {"m5.large": {"0.0495", "N/A*"}},
	})
	b = appendOK(t, b, later, AppendStats{Deleted: 1, ODRows: 3})
	m5 := b.Instances["m5.large"]
	if n := len(m5.OD); n != 5 {
		t.Errorf("m5.large has %d on-demand rows, want 5: %+v", n, m5.OD)
	}

	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Offer       string   `json:"offer"`
		VariantKeys []string `json:"variant_keys"`
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Offer != spotOffer || len(raw.VariantKeys) != 1 || raw.VariantKeys[0] != "operatingSystem" {
		t.Errorf("offer %q, variant keys %v", raw.Offer, raw.VariantKeys)
	}

	if _, err := b.Append(later); !errors.Is(err, errNotNewer) {
		t.Errorf("appending the same feed again: %v, want errNotNewer", err)
	}
	ec2 := pricing("2026-12-01T00:00:00Z", false, false).snapshot(t)
	if _, err := b.Append(ec2); err == nil {
		t.Error("appending an EC2 price list to a spot bundle should fail")
	}
}

func TestSpotErrors(t *testing.T) {
	for name, feed := range map[string]string{
		"no regions": `{"vers":0.01,"config":{"regions":[]}}`,
		"bad price":  `{"vers":0.01,"config":{"regions":[{"region":"us-east-1","instanceTypes":[{"sizes":[{"size":"m5.large","valueColumns":[{"name":"linux","prices":{"USD":"cheap"}}]}]}]}]}}`,
		"no prices":  `{"vers":0.01,"config":{"regions":[{"region":"us-east-1","instanceTypes":[{"sizes":[{"size":"m5.large","valueColumns":[{"name":"linux","prices":{"USD":"N/A*"}}]}]}]}]}}`,
	} {
		if _, err := readSpot(bytes.NewReader([]byte(feed)), 1); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if isSpotFeed([]byte(`{"formatVersion":"v1.0","disclaimer":"...","offerCode":"AmazonEC2"`)) {
		t.Error("a price list should not be taken for a spot price feed")
	}
}

// writeHistory saves describe-spot-price-history pages for one region as seed-spot.sh does.
func writeHistory(t *testing.T, dir, region string, done bool, pages ...[]spotHistoryRecord) {
	t.Helper()
	rd := filepath.Join(dir, region)
	if err := os.MkdirAll(rd, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, recs := range pages {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if err := json.NewEncoder(zw).Encode(map[string]any{"SpotPriceHistory": recs, "NextToken": ""}); err != nil {
			t.Fatal(err)
		}
		zw.Close()
		if err := os.WriteFile(filepath.Join(rd, fmt.Sprintf("%06d.json.gz", i+1)), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if done {
		if err := os.WriteFile(filepath.Join(rd, "done"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpotHistory(t *testing.T) {
	rec := func(az, itype, product, price, ts string) spotHistoryRecord {
		return spotHistoryRecord{az, itype, product, price, ts}
	}
	dir := t.TempDir()
	writeHistory(t, dir, "us-east-1", true, []spotHistoryRecord{
		// In effect before the history starts, so counted from the start.
		rec("us-east-1a", "m5.large", "Linux/UNIX", "0.040000", "2026-07-30T00:00:00+00:00"),
		rec("us-east-1b", "m5.large", "Linux/UNIX", "0.045000", "2026-07-31T12:00:00+00:00"),
		// 1b drops below 1a: the region's lowest price changes.
		rec("us-east-1b", "m5.large", "Linux/UNIX", "0.038000", "2026-08-10T10:20:00+00:00"),
		// 1a rises, but 1b is still lowest: no change.
		rec("us-east-1a", "m5.large", "Linux/UNIX", "0.041000", "2026-08-10T10:40:00+00:00"),
	}, []spotHistoryRecord{
		rec("us-east-1-atl-1a", "m5.large", "Linux/UNIX (Amazon VPC)", "0.050000", "2026-09-01T00:00:00+00:00"),
		rec("us-east-1a", "m5.large", "SUSE Linux", "0.140000", "2026-08-01T00:00:00+00:00"),
		rec("us-east-1a", "c5.large", "Windows", "0.120000", "2026-08-02T00:00:00.000Z"),
		// After the feed was published: left for the feed.
		rec("us-east-1a", "m5.large", "Linux/UNIX", "0.010000", "2026-11-01T10:00:00+00:00"),
	})
	start, end := mustTS(t, "2026-08-01T00:00:00Z"), mustTS(t, "2026-11-01T09:00:00Z")
	h, err := readSpotHistory(dir, start, end, "min")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Events) != 4 {
		t.Fatalf("%d events, want 4: %+v", len(h.Events), h.Events)
	}

	b := &Bundle{Instances: map[string]*Instance{}}
	st, n, err := appendSpotHistory(b, h, start, end, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 || st.Created != 2 || st.ODRows != 4 {
		t.Errorf("%d snapshots, stats %+v; want 4 snapshots, 2 created, 4 rows", n, st)
	}
	linux := odKey{"us-east-1", Variant{"Linux"}}
	var times []string
	for _, r := range b.Instances["m5.large"].OD {
		if r.Region == "us-east-1" {
			times = append(times, fmtTS(r.TS))
		}
	}
	if want := []string{"2026-08-01T00:00:00Z", "2026-08-10T10:20:00Z"}; !slices.Equal(times, want) {
		t.Errorf("us-east-1 m5.large rows at %v, want %v", times, want)
	}
	if got := b.Instances["m5.large"].LiveOD(); got[linux] != 0.038 || got[odKey{"us-east-1-atl-1", Variant{"Linux"}}] != 0.05 {
		t.Errorf("m5.large prices %v", got)
	}

	feed := spotSnapshot(t, "2026-11-01T09:00:00Z", map[string]map[string][2]string{
		"us-east-1": {"m5.large": {"0.038", "N/A*"}, "c5.large": {"0.03", "0.12"}},
		"eu-west-1": {"m5.large": {"0.0495", "N/A*"}},
	})
	report := compareSpot(h, feed, "min")
	for _, want := range []string{"for 2 prices in both", "min    100.00% match (used)", "max     50.00% match", "eu-west-1 (1)", "us-east-1 (1)"} {
		if !strings.Contains(report, want) {
			t.Errorf("comparison should mention %q:\n%s", want, report)
		}
	}
	// c5.large gets a Linux price, eu-west-1 is new, and the Local Zone price the feed lacks is removed.
	appendOK(t, b, feed, AppendStats{ODRows: 3, RegionChanges: 2})

	writeHistory(t, dir, "eu-west-1", false, []spotHistoryRecord{rec("eu-west-1a", "m5.large", "Linux/UNIX", "0.04", "2026-08-02T00:00:00Z")})
	if _, err := readSpotHistory(dir, start, end, "min"); err == nil {
		t.Error("a region that was not fetched completely should be an error")
	}
}

func TestSpotRegionAttrs(t *testing.T) {
	for code, want := range map[string]string{
		"us-east-1":               "AWS Region",
		"ap-southeast-7":          "AWS Region",
		"us-east-1-atl-1":         "AWS Local Zone",
		"ap-southeast-1-han-1":    "AWS Local Zone",
		"us-east-1-wl1":           "AWS Wavelength Zone",
		"us-east-1-wl1-bos-wlz-1": "AWS Wavelength Zone",
	} {
		got := spotRegionAttrs(code)
		if got["locationType"] != want || got["regionCode"] != code {
			t.Errorf("spotRegionAttrs(%q) = %v, want locationType %q", code, got, want)
		}
	}
}
