package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"testing"
)

var regionCodes = map[string]string{
	"US East (N. Virginia)": "us-east-1", "EU (Ireland)": "eu-west-1",
	"Asia Pacific (Tokyo)": "ap-northeast-1", "South America (Sao Paulo)": "sa-east-1",
}

type fixture struct {
	pub      string
	products map[string]awsProduct
	terms    map[string]map[string]map[string]awsOffer // term type -> sku -> offer code -> offer
}

func newFixture(pub string) *fixture {
	return &fixture{pub: pub, products: map[string]awsProduct{}, terms: map[string]map[string]map[string]awsOffer{
		"OnDemand": {}, "Reserved": {},
	}}
}

// product adds an instance SKU priced on demand; extra holds attribute overrides as key, value pairs.
func (f *fixture) product(sku, family, itype, region, os, eff, price string, extra ...string) {
	attrs := map[string]string{
		"instanceType": itype, "location": region, "operatingSystem": os, "tenancy": "Shared",
		"licenseModel": "No License required", "preInstalledSw": "NA", "capacitystatus": "Used",
		"usagetype": "BoxUsage:" + sku, "vcpu": "2", "memory": "8 GiB", "physicalProcessor": "Intel Xeon Platinum 8175",
		"regionCode": regionCodes[region], "locationType": "AWS Region",
	}
	for i := 0; i+1 < len(extra); i += 2 {
		attrs[extra[i]] = extra[i+1]
	}
	f.products[sku] = awsProduct{ProductFamily: family, Attributes: attrs}
	f.terms["OnDemand"][sku] = map[string]awsOffer{sku + ".JRTCKXETXF": {
		EffectiveDate:   eff,
		PriceDimensions: map[string]awsDimension{"d": {Unit: "Hrs", PricePerUnit: map[string]string{"USD": price}}},
	}}
}

func (f *fixture) reserved(sku, code, eff, lease, option, hourly, upfront string) {
	dims := map[string]awsDimension{"h": {Unit: "Hrs", PricePerUnit: map[string]string{"USD": hourly}}}
	if upfront != "" {
		dims["u"] = awsDimension{Unit: "Quantity", PricePerUnit: map[string]string{"USD": upfront}}
	}
	if f.terms["Reserved"][sku] == nil {
		f.terms["Reserved"][sku] = map[string]awsOffer{}
	}
	f.terms["Reserved"][sku][sku+"."+code] = awsOffer{
		EffectiveDate:   eff,
		PriceDimensions: dims,
		TermAttributes:  map[string]string{"LeaseContractLength": lease, "OfferingClass": "standard", "PurchaseOption": option},
	}
}

func (f *fixture) snapshot(t *testing.T) *snapshot {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"formatVersion": "v1.0", "publicationDate": f.pub, "products": f.products, "terms": f.terms,
		"zzz": "unknown keys are skipped",
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := readAWS(bytes.NewReader(data), ".host")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// pricing builds a fixture; changed picks the second month's prices, withC4 keeps c4.large listed.
func pricing(pub string, changed, withC4 bool) *fixture {
	f := newFixture(pub)
	memory := "8 GiB"
	if changed {
		memory = "8.0 GiB"
	}
	if changed {
		f.product("A", "Compute Instance", "m5.large", "US East (N. Virginia)", "Linux", "2020-01-15T00:00:00Z", "0.09", "memory", memory)
	} else {
		f.product("A", "Compute Instance", "m5.large", "US East (N. Virginia)", "Linux", "2019-12-01T00:00:00Z", "0.096", "memory", memory)
	}
	f.product("B", "Compute Instance", "m5.large", "EU (Ireland)", "Linux", "2019-12-01T00:00:00Z", "0.107", "memory", memory)
	if !changed {
		f.product("C", "Compute Instance", "m5.large", "EU (Ireland)", "Windows", "2019-12-01T00:00:00Z", "0.188", "memory", memory)
	} else {
		f.product("G", "Compute Instance", "m5.large", "South America (Sao Paulo)", "Linux", "2020-01-20T00:00:00Z", "0.15", "memory", memory)
	}
	f.product("D", "Compute Instance", "m5.large", "Asia Pacific (Tokyo)", "Linux", "2019-12-01T00:00:00Z", "0.124",
		"memory", memory, "physicalProcessor", "Intel Xeon Platinum 8259CL")
	if changed {
		f.reserved("A", "1", "2020-01-15T00:00:00Z", "1yr", "PartialUpfront", "0.028", "290")
	} else {
		f.reserved("A", "1", "2019-12-01T00:00:00Z", "1yr", "PartialUpfront", "0.03", "300")
	}
	f.reserved("A", "2", "2019-12-01T00:00:00Z", "3 yr", "No Upfront", "0.05", "")
	if withC4 {
		f.product("E", "Compute Instance", "c4.large", "US East (N. Virginia)", "Linux", "2019-12-01T00:00:00Z", "0.1")
	}
	f.product("F", "Dedicated Host", "m5", "US East (N. Virginia)", "Linux", "2019-12-01T00:00:00Z", "4.5",
		"tenancy", "Host", "capacitystatus", "AllocatedHost")
	// Non-instance products are dropped along with their terms.
	f.product("X", "Data Transfer", "", "US East (N. Virginia)", "", "2019-12-01T00:00:00Z", "0.02")
	return f
}

func assertMatches(t *testing.T, b *Bundle, s *snapshot) {
	t.Helper()
	for name, si := range s.Instances {
		inst := b.Instances[name]
		if inst == nil {
			t.Errorf("%s: missing from bundle", name)
			continue
		}
		if inst.DeletedAt != nil {
			t.Errorf("%s: marked deleted but listed in the file", name)
		}
		for region, want := range si.Attrs {
			if got := inst.AttrsIn(region); !maps.Equal(got, want) {
				t.Errorf("%s in %s: attrs %v, want %v", name, region, got, want)
			}
		}
		wantOD := map[odKey]float64{}
		for k, p := range si.OD {
			wantOD[k] = p.Price
		}
		if got := inst.LiveOD(); !maps.Equal(got, wantOD) {
			t.Errorf("%s: on-demand %v, want %v", name, got, wantOD)
		}
		wantRS := map[rsKey][2]float64{}
		for k, p := range si.RS {
			wantRS[k] = [2]float64{p.Hourly, p.Upfront}
		}
		if got := inst.LiveRS(); !maps.Equal(got, wantRS) {
			t.Errorf("%s: reserved %v, want %v", name, got, wantRS)
		}
	}
	for name, inst := range b.Instances {
		if _, ok := s.Instances[name]; ok {
			continue
		}
		if inst.DeletedAt == nil {
			t.Errorf("%s: not in the file but not marked deleted", name)
		}
		if n := len(inst.LiveOD()) + len(inst.LiveRS()); n > 0 {
			t.Errorf("%s: deleted but still has %d live prices", name, n)
		}
	}
	for loc, want := range s.Regions {
		if got := b.Regions[loc]; !maps.Equal(got, want) {
			t.Errorf("region %s: %v, want %v", loc, got, want)
		}
	}
	for name, inst := range b.Instances {
		if inst.hasRegionAttrs() {
			t.Errorf("%s: region attributes should live in the regions table", name)
		}
	}
}

// roundTrip writes and rereads the bundle, checking the encoding is stable.
func roundTrip(t *testing.T, b *Bundle) *Bundle {
	t.Helper()
	var first, second bytes.Buffer
	if err := b.Write(&first); err != nil {
		t.Fatal(err)
	}
	back, err := ReadBundle(bytes.NewReader(first.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := back.Write(&second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("rewriting a decoded bundle changed it:\n%s\n%s", first.String(), second.String())
	}
	return back
}

func appendOK(t *testing.T, b *Bundle, s *snapshot, want AppendStats) *Bundle {
	t.Helper()
	got, err := b.Append(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("stats %+v, want %+v", got, want)
	}
	assertMatches(t, b, s)
	b = roundTrip(t, b)
	assertMatches(t, b, s)
	return b
}

func TestHistory(t *testing.T) {
	b := &Bundle{Instances: map[string]*Instance{}}

	jan := pricing("2020-01-01T00:00:00Z", false, true).snapshot(t)
	b = appendOK(t, b, jan, AppendStats{Created: 3, AttrChanges: 1, ODRows: 6, RSRows: 2, RegionChanges: 6})
	if got := b.Regions["EU (Ireland)"]; got["regionCode"] != "eu-west-1" || got["locationType"] != "AWS Region" {
		t.Errorf("Ireland region attributes = %v", got)
	}
	m5 := b.Instances["m5.large"]
	if m5.Attrs["physicalProcessor"] != "Intel Xeon Platinum 8175" || len(m5.AttrChanges) != 1 || m5.AttrChanges[0].Region != "Asia Pacific (Tokyo)" {
		t.Errorf("Tokyo's processor should be the only regional override: base %q, changes %+v", m5.Attrs["physicalProcessor"], m5.AttrChanges)
	}
	if b.Instances["m5.host"] == nil {
		t.Error("Dedicated Host m5 should be named m5.host")
	}
	if _, ok := m5.LiveRS()[rsKey{"US East (N. Virginia)", Variant{"Linux", "Shared", "No License required", "NA", "Used"}, Term{"3yr", "standard", "No Upfront"}}]; !ok {
		t.Errorf("reserved terms should be normalized, got %v", m5.LiveRS())
	}

	// Feb: Virginia price and 1yr reserved change, Ireland Windows goes away, Sao Paulo is added,
	// memory is respelled everywhere, and c4.large is no longer listed.
	feb := pricing("2020-02-01T00:00:00Z", true, false).snapshot(t)
	b = appendOK(t, b, feb, AppendStats{Deleted: 1, AttrChanges: 1, ODRows: 4, RSRows: 1, RegionChanges: 2})
	m5 = b.Instances["m5.large"]
	if c := m5.AttrChanges[len(m5.AttrChanges)-1]; c.Region != "" || c.Key != "memory" {
		t.Errorf("memory should change globally, got %+v", c)
	}
	var virginia []ODRow
	for _, r := range m5.OD {
		if r.Region == "US East (N. Virginia)" {
			virginia = append(virginia, r)
		}
	}
	if len(virginia) != 2 || virginia[1].TS != mustTS(t, "2020-01-15T00:00:00Z") {
		t.Errorf("Virginia's new price should be dated by its effectiveDate: %+v", virginia)
	}
	if d := b.Instances["c4.large"].DeletedAt; d == nil || *d != feb.TS {
		t.Errorf("c4.large deleted_at = %v, want %d", d, feb.TS)
	}

	mar := pricing("2020-03-01T00:00:00Z", true, true).snapshot(t)
	b = appendOK(t, b, mar, AppendStats{Restored: 1, ODRows: 1})
	if c4 := b.Instances["c4.large"]; c4.CreatedAt != jan.TS {
		t.Errorf("restoring c4.large should keep created_at %d, got %d", jan.TS, c4.CreatedAt)
	}

	apr := pricing("2020-04-01T00:00:00Z", true, true).snapshot(t)
	b = appendOK(t, b, apr, AppendStats{})

	if _, err := b.Append(feb); !errors.Is(err, errNotNewer) {
		t.Errorf("appending an older file should fail with errNotNewer, got %v", err)
	}
}

// TestUpgrade reads a bundle written before the regions table, where region attributes sat on instances.
func TestUpgrade(t *testing.T) {
	str := func(s string) *string { return &s }
	linux := Variant{"Linux", "Shared", "No License required", "NA", "Used"}
	deleted := int64(300)
	old := &Bundle{Snapshots: []int64{100, 200, 300}, Instances: map[string]*Instance{
		"m5.large": {
			Attrs: map[string]string{"vcpu": "2", "regionCode": "us-east-1", "locationType": "AWS Region"},
			AttrChanges: []AttrChange{
				{TS: 200, Key: "regionCode", Value: str("eu-west-1"), Region: "EU (Ireland)"},
				{TS: 200, Key: "vcpu", Value: str("4"), Region: "EU (Ireland)"},
			},
			BasePrice: 0.096,
			OD: []ODRow{
				{TS: 50, Region: "US East (N. Virginia)", Variant: linux},
				{TS: 50, Region: "EU (Ireland)", Variant: linux},
				// Left Paris before regionCode existed, so the base us-east-1 must not be read as Paris's code.
				{TS: 50, Region: "EU (Paris)", Variant: linux},
				{TS: 150, Region: "EU (Paris)", Variant: linux, Removed: true},
			},
			CreatedAt: 100,
		},
		"m1.small": {
			Attrs:       map[string]string{"regionCode": "us-east-1"},
			AttrChanges: []AttrChange{{TS: 200, Key: "regionCode", Value: str("ap-northeast-1"), Region: "Asia Pacific (Tokyo)"}},
			BasePrice:   0.044,
			OD: []ODRow{
				{TS: 50, Region: "Asia Pacific (Tokyo)", Variant: linux},
				{TS: 300, Region: "Asia Pacific (Tokyo)", Variant: linux, Removed: true},
			},
			CreatedAt: 100,
			DeletedAt: &deleted,
		},
	}}
	var buf bytes.Buffer
	if err := old.Write(&buf); err != nil {
		t.Fatal(err)
	}
	b, err := ReadBundle(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if b.lifted != (liftStats{Rows: 5}) {
		t.Errorf("lifted %+v, want 5 rows and no conflicts", b.lifted)
	}
	want := map[string]map[string]string{
		"US East (N. Virginia)": {"regionCode": "us-east-1", "locationType": "AWS Region"},
		"EU (Ireland)":          {"regionCode": "eu-west-1", "locationType": "AWS Region"},
		"Asia Pacific (Tokyo)":  {"regionCode": "ap-northeast-1"},
	}
	if !maps.EqualFunc(b.Regions, want, maps.Equal) {
		t.Errorf("regions %v, want %v", b.Regions, want)
	}
	m5 := b.Instances["m5.large"]
	if got := m5.AttrsIn("EU (Ireland)"); !maps.Equal(got, map[string]string{"vcpu": "4"}) {
		t.Errorf("other attributes should be untouched, Ireland has %v", got)
	}
	b = roundTrip(t, b)
	if b.lifted != (liftStats{}) || !maps.EqualFunc(b.Regions, want, maps.Equal) {
		t.Errorf("an upgraded bundle should read back unchanged: lifted %+v, regions %v", b.lifted, b.Regions)
	}
}

func TestRemovedAttribute(t *testing.T) {
	b := &Bundle{Instances: map[string]*Instance{}}
	f := newFixture("2020-01-01T00:00:00Z")
	f.product("A", "Compute Instance", "m5.large", "US East (N. Virginia)", "Linux", "2019-12-01T00:00:00Z", "0.096", "ecu", "8")
	f.product("B", "Compute Instance", "m5.large", "EU (Ireland)", "Linux", "2019-12-01T00:00:00Z", "0.107", "ecu", "8")
	b = appendOK(t, b, f.snapshot(t), AppendStats{Created: 1, ODRows: 2, RegionChanges: 4})

	f.pub = "2020-02-01T00:00:00Z"
	delete(f.products["B"].Attributes, "ecu")
	b = appendOK(t, b, f.snapshot(t), AppendStats{AttrChanges: 1})
	if c := b.Instances["m5.large"].AttrChanges[0]; c.Value != nil || c.Region != "EU (Ireland)" {
		t.Errorf("want ecu removed in Ireland only, got %+v", c)
	}
}

func mustTS(t *testing.T, s string) int64 {
	t.Helper()
	ts, err := parseTS(s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
