package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// --- Azure ---

func azItem(sku, region, product, skuName, typ string, price float64) azureItem {
	return azureItem{ArmSkuName: sku, ArmRegionName: region, Location: "Region " + region, ProductName: product, SkuName: skuName,
		Type: typ, RetailPrice: price, CurrencyCode: "USD", UnitOfMeasure: "1 Hour", EffectiveStartDate: "2024-01-01T00:00:00Z"}
}

func azureFile(t *testing.T, fetched string, items []azureItem) *snapshot {
	t.Helper()
	src := azureSource{Kind: azureSourceKind, FetchedAt: fetched, Items: items, Specs: map[string]map[string]any{
		"linux-d2sv3-standard":    {"cores": json.Number("2"), "ram": json.Number("8.0"), "series": "Dsv3", "isVcpu": true},
		"windows-d2sv3-standard":  {"cores": json.Number("2"), "ram": json.Number("8.0"), "series": "Dsv3 Windows"},
		"linux-e4-2s-v3-standard": {"cores": json.Number("2"), "ram": json.Number("32.0"), "series": "Esv3"},
		"sql-d2sv3-enterprise":    {"cores": json.Number("99")},
	}}
	data, _ := json.Marshal(src)
	if sourceKind(data) != azureSourceKind {
		t.Fatal("fixture is not recognized as an Azure file")
	}
	s, err := readAzure(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func azureItems(d2Linux float64) []azureItem {
	linux := azItem("Standard_D2s_v3", "eastus", "Virtual Machines DSv3 Series", "D2s v3", "Consumption", d2Linux)
	linux.SavingsPlan = []azureSavingsPlan{{RetailPrice: 0.066, Term: "1 Year"}, {RetailPrice: 0.045, Term: "3 Years"}}
	reservation := azItem("Standard_D2s_v3", "eastus", "Virtual Machines DSv3 Series", "D2s v3", "Reservation", 500)
	reservation.ReservationTerm = "1 Year"
	future := azItem("Standard_E4-2s_v3", "westeurope", "Virtual Machines ESv3 Series Win", "E4-2s v3", "Consumption", 0.4)
	future.EffectiveStartDate = "2099-01-01T00:00:00Z"
	return []azureItem{
		linux, reservation, future,
		azItem("Standard_D2s_v3", "eastus", "Virtual Machines DSv3 Series Windows", "D2s v3", "Consumption", 0.188),
		azItem("Standard_D2s_v3", "eastus", "Virtual Machines DSv3 Series", "D2s v3 Spot", "Consumption", 0.019),
		azItem("Standard_D2s_v3", "eastus", "Virtual Machines DSv3 Series Windows VM", "D2s v3 Low Priority", "Consumption", 0.075),
		azItem("Standard_D2s_v3", "westus", "Virtual Machines DSv3 Series", "D2s v3", "Consumption", 0.1),
		// Not VM sizes, so left out without a warning.
		azItem("Dsv3_Type1", "eastus", "Dsv3 Series Dedicated Host", "Dsv3 Type1", "Consumption", 5),
		azItem("Standard_D2s_v3", "eastus", "Dsv3 Series CloudServices", "D2s v3", "Consumption", 9),
		// No ARM name: counted and left out.
		azItem("", "eastus", "Virtual Machines RI", "x", "Reservation", 1),
	}
}

func TestAzure(t *testing.T) {
	s1 := azureFile(t, "2026-11-01T00:00:00Z", azureItems(0.096))
	if len(s1.Instances) != 2 || s1.Skipped != 1 || s1.Products != 8 || s1.Collisions != 0 {
		t.Fatalf("%d instances, %d skipped, %d products, %d collisions", len(s1.Instances), s1.Skipped, s1.Products, s1.Collisions)
	}
	d2 := s1.Instances["Standard_D2s_v3"]
	want := map[odKey]float64{
		{"eastus", Variant{"Linux", azureRegular}}:       0.096,
		{"eastus", Variant{"Windows", azureRegular}}:     0.188,
		{"eastus", Variant{"Linux", azureSpot}}:          0.019,
		{"eastus", Variant{"Windows", azureLowPriority}}: 0.075,
		{"westus", Variant{"Linux", azureRegular}}:       0.1,
	}
	for k, p := range want {
		if got := d2.OD[k]; got.Price != p {
			t.Errorf("D2s_v3 %v = %v, want %v", k, got.Price, p)
		}
	}
	if len(d2.OD) != len(want) {
		t.Errorf("D2s_v3 on-demand %v", d2.OD)
	}
	linux := Variant{"Linux", azureRegular}
	if p := d2.RS[rsKey{"eastus", linux, Term{"1yr", "Reserved Instance", "All Upfront"}}]; p.Hourly != 0 || p.Upfront != 500 {
		t.Errorf("reservation %v", p)
	}
	if p := d2.RS[rsKey{"eastus", linux, Term{"3yr", "Savings Plan", "No Upfront"}}]; p.Hourly != 0.045 {
		t.Errorf("savings plan %v", p)
	}
	if d2.OD[odKey{"eastus", linux}].Eff != 1704067200 {
		t.Errorf("price should be dated by its effectiveStartDate: %v", d2.OD[odKey{"eastus", linux}])
	}
	attrs := d2.Attrs["eastus"]
	for k, v := range map[string]string{"cores": "2", "ram": "8.0", "series": "Dsv3", "isVcpu": "true", "skuName": "D2s v3",
		"productName": "Virtual Machines DSv3 Series", "productFamily": "Virtual Machines"} {
		if attrs[k] != v {
			t.Errorf("D2s_v3 %s = %q, want %q", k, attrs[k], v)
		}
	}
	e4 := s1.Instances["Standard_E4-2s_v3"]
	if p := e4.OD[odKey{"westeurope", Variant{"Windows", azureRegular}}]; p.Price != 0.4 || p.Eff != s1.TS {
		t.Errorf("a future effectiveStartDate should be capped at the fetch: %v", p)
	}
	if e4.Attrs["westeurope"]["ram"] != "32.0" {
		t.Errorf("E4-2s_v3 should match the calculator's e4-2s-v3: %v", e4.Attrs)
	}
	if r := s1.Regions["eastus"]; r["name"] != "Region eastus" || r["locationType"] != "Azure Region" {
		t.Errorf("region %v", r)
	}

	b := &Bundle{Instances: map[string]*Instance{}}
	appendOK(t, b, s1)
	before := roundTrip(t, b)
	if _, err := b.Append(azureFile(t, "2026-11-01T01:00:00Z", azureItems(0.096))); !errors.Is(err, errUnchanged) {
		t.Fatalf("unchanged fetch: %v", err)
	}
	if !reflect.DeepEqual(b, before) {
		t.Fatal("an unchanged fetch modified the bundle")
	}
	st := appendOK(t, b, azureFile(t, "2026-11-01T02:00:00Z", azureItems(0.09)))
	if st.ODRows != 1 || st.RSRows != 0 || len(b.Snapshots) != 2 {
		t.Errorf("stats %+v, %d snapshots", st, len(b.Snapshots))
	}
	// The new price keeps its old effectiveStartDate, which is before the previous row, so it is dated by the fetch.
	for _, r := range b.Instances["Standard_D2s_v3"].OD {
		if r.Region == "eastus" && r.Variant == linux && r.Price != nil && *r.Price == 0.09 && r.TS != b.Snapshots[1] {
			t.Errorf("changed price dated %d, want the fetch %d", r.TS, b.Snapshots[1])
		}
	}
}

func TestAzureTerms(t *testing.T) {
	for in, want := range map[string]string{"1 Year": "1yr", "3 Years": "3yr", "5 Years": "5yr"} {
		if got, err := azureLease(in); got != want || err != nil {
			t.Errorf("azureLease(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"1 Month", "Years", "0 Years"} {
		if _, err := azureLease(in); err == nil {
			t.Errorf("azureLease(%q) should fail", in)
		}
	}
	for arm, want := range map[string]string{"Standard_D2s_v3": "standard:d2sv3", "Standard_E4-2s_v3": "standard:e42sv3",
		"Basic_A1": "basic:a1", "Standard_DS3_v2_Promo": "standard:ds3v2"} {
		if got := azureArmSpecKey(arm); got != want {
			t.Errorf("azureArmSpecKey(%q) = %q, want %q", arm, got, want)
		}
	}
}

// azureFakeAPI serves items like the Retail Prices API: OData filters made of " and "-joined conditions, at
// most maxClauses of them, and pageSize items per page linked by $skip.
func azureFakeAPI(t *testing.T, items []azureItem, pageSize, maxClauses int) (*httptest.Server, *atomic.Int64) {
	var requests atomic.Int64
	field := func(it azureItem, name string) string {
		switch name {
		case "serviceName":
			return "Virtual Machines"
		case "priceType":
			return it.Type
		case "armSkuName":
			return it.ArmSkuName
		case "productName":
			return it.ProductName
		case "skuName":
			return it.SkuName
		}
		t.Errorf("filter on unknown field %q", name)
		return ""
	}
	match := func(it azureItem, cond string) bool {
		want := true
		if c, ok := strings.CutSuffix(cond, " eq false"); ok {
			cond, want = c, false
		}
		for _, fn := range []string{"startswith", "contains"} {
			if args, ok := strings.CutPrefix(cond, fn+"("); ok {
				name, val, _ := strings.Cut(strings.TrimSuffix(args, ")"), ",")
				val = strings.Trim(val, "'")
				f := field(it, name)
				got := strings.Contains(f, val)
				if fn == "startswith" {
					got = strings.HasPrefix(f, val)
				}
				return got == want
			}
		}
		parts := strings.SplitN(cond, " ", 3)
		if len(parts) != 3 || !want {
			t.Errorf("can't read condition %q", cond)
			return false
		}
		eq := field(it, parts[0]) == strings.Trim(parts[2], "'")
		return eq == (parts[1] == "eq")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		q := r.URL.Query()
		conds := strings.Split(q.Get("$filter"), " and ")
		if len(conds) > maxClauses {
			http.Error(w, `{"Error":{"Message":"Invalid OData parameters supplied"}}`, http.StatusBadRequest)
			return
		}
		var matched []azureItem
		for _, it := range items {
			ok := true
			for _, c := range conds {
				ok = ok && match(it, c)
			}
			if ok {
				matched = append(matched, it)
			}
		}
		skip, _ := strconv.Atoi(q.Get("$skip"))
		end := min(skip+pageSize, len(matched))
		out := map[string]any{"Items": matched[min(skip, end):end], "NextPageLink": nil}
		if end < len(matched) {
			q.Set("$skip", strconv.Itoa(end))
			out["NextPageLink"] = "http://" + r.Host + r.URL.Path + "?" + q.Encode()
		}
		json.NewEncoder(w).Encode(out)
	}))
	return srv, &requests
}

func TestAzureFetchSplits(t *testing.T) {
	var items []azureItem
	for i, sku := range []string{"Standard_D2s_v3", "Standard_D4s_v3", "Standard_D16s_v5", "Standard_DC2s", "Standard_E2s_v3",
		"Standard_E8s_v3", "Standard_E48s_v3", "Standard_F2s", "Standard_NC6", "Standard_A1_v2", "Standard_B2s", "Basic_A0", "Standard_ND40rs_v2"} {
		for _, region := range []string{"eastus", "westus", "northeurope"} {
			items = append(items,
				azItem(sku, region, "Virtual Machines", strings.TrimPrefix(sku, "Standard_"), "Consumption", float64(i)),
				azItem(sku, region, "Virtual Machines Windows", strings.TrimPrefix(sku, "Standard_"), "Consumption", float64(i)),
				azItem(sku, region, "Virtual Machines", sku+" Spot", "Consumption", float64(i)),
				azItem(sku, region, "Virtual Machines", sku+" Low Priority", "Consumption", float64(i)),
				azItem(sku, region, "Virtual Machines", sku, "Reservation", float64(i)))
		}
	}
	items = append(items, azItem("", "eastus", "Virtual Machines RI", "x", "Reservation", 1))
	// The filter excludes DevTest prices.
	items = append(items, azItem("Standard_D2s_v3", "eastus", "Virtual Machines", "D2s v3", "DevTestConsumption", 1))

	srv, requests := azureFakeAPI(t, items, 4, 12)
	defer srv.Close()
	got, err := fetchAzureItems(context.Background(), srv.Client(), srv.URL, 4)
	if err != nil {
		t.Fatal(err)
	}
	key := func(it azureItem) string {
		return fmt.Sprintf("%s|%s|%s|%s|%s", it.ArmSkuName, it.ArmRegionName, it.ProductName, it.SkuName, it.Type)
	}
	var gotKeys, wantKeys []string
	for _, it := range got {
		gotKeys = append(gotKeys, key(it))
	}
	for _, it := range items[:len(items)-1] {
		wantKeys = append(wantKeys, key(it))
	}
	slices.Sort(gotKeys)
	slices.Sort(wantKeys)
	if !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("fetched %d prices, want %d", len(gotKeys), len(wantKeys))
	}
	if n := requests.Load(); n < int64(len(items)/4) {
		t.Errorf("only %d requests", n)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer fail.Close()
	if _, err := fetchAzureItems(context.Background(), fail.Client(), fail.URL, 4); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("forbidden: %v", err)
	}
}

// --- GCP ---

func gcpSkuFixture(desc, group, usage, unit string, price float64, regions ...string) gcpSku {
	var s gcpSku
	s.Description = desc
	s.Category.ResourceFamily = "Compute"
	s.Category.ResourceGroup = group
	s.Category.UsageType = usage
	s.ServiceRegions = regions
	s.PricingInfo = make([]struct {
		PricingExpression struct {
			UsageUnit   string `json:"usageUnit"`
			TieredRates []struct {
				StartUsageAmount float64 `json:"startUsageAmount"`
				UnitPrice        struct {
					CurrencyCode string `json:"currencyCode"`
					Units        string `json:"units"`
					Nanos        int64  `json:"nanos"`
				} `json:"unitPrice"`
			} `json:"tieredRates"`
		} `json:"pricingExpression"`
	}, 1)
	e := &s.PricingInfo[0].PricingExpression
	e.UsageUnit = unit
	e.TieredRates = make([]struct {
		StartUsageAmount float64 `json:"startUsageAmount"`
		UnitPrice        struct {
			CurrencyCode string `json:"currencyCode"`
			Units        string `json:"units"`
			Nanos        int64  `json:"nanos"`
		} `json:"unitPrice"`
	}, 1)
	units := int64(price)
	e.TieredRates[0].UnitPrice.CurrencyCode = "USD"
	e.TieredRates[0].UnitPrice.Units = strconv.FormatInt(units, 10)
	e.TieredRates[0].UnitPrice.Nanos = int64((price-float64(units))*1e9 + 0.5)
	return s
}

func gcpSkus(n2Core float64) []gcpSku {
	return []gcpSku{
		gcpSkuFixture("N2 Instance Core running in Americas", "CPU", "OnDemand", "h", n2Core, "us-central1", "us-east1"),
		gcpSkuFixture("N2 Instance Ram running in Americas", "RAM", "OnDemand", "GiBy.h", 0.004237, "us-central1", "us-east1"),
		gcpSkuFixture("Spot Preemptible N2 Instance Core running in Americas", "CPU", "Preemptible", "h", 0.01896, "us-central1"),
		gcpSkuFixture("Spot Preemptible N2 Instance Ram running in Americas", "RAM", "Preemptible", "GiBy.h", 0.002542, "us-central1"),
		gcpSkuFixture("Commitment v1: N2 Cpu in Americas for 1 Year", "CPU", "Commit1Yr", "h", 0.019915, "us-central1"),
		gcpSkuFixture("Commitment v1: N2 Ram in Americas for 1 Year", "RAM", "Commit1Yr", "GiBy.h", 0.002669, "us-central1"),
		gcpSkuFixture("Commitment v1: Cpu in Americas for 1 Year", "CPU", "Commit1Yr", "h", 0.019915, "us-central1"),
		gcpSkuFixture("C4D Instance Core running in Americas", "CPU", "OnDemand", "h", 0.03, "us-central1"),
		gcpSkuFixture("C4D Instance Ram running in Americas", "RAM", "OnDemand", "GBy.h", 0.001, "us-central1"),
		gcpSkuFixture("C4D Custom Instance Core running in Americas", "CPU", "OnDemand", "h", 9, "us-central1"),
		gcpSkuFixture("Sole Tenancy Premium for C4D Sole Tenancy Instance Core running in Americas", "CPU", "OnDemand", "h", 9, "us-central1"),
		gcpSkuFixture("C4D Instance Local SSD running in Americas", "LocalSSD", "OnDemand", "GiBy.mo", 0.16, "us-central1"),
		gcpSkuFixture("E2 Instance Core running in Americas", "CPU", "OnDemand", "h", 0.02181159, "us-central1"),
		gcpSkuFixture("E2 Instance Ram running in Americas", "RAM", "OnDemand", "GiBy.h", 0.00292353, "us-central1"),
		gcpSkuFixture("Micro Instance with burstable CPU running in Americas", "F1Micro", "OnDemand", "h", 0.0076, "us-central1"),
		gcpSkuFixture("A2 Instance Core running in Americas", "CPU", "OnDemand", "h", 0.031611, "us-central1"),
		gcpSkuFixture("A2 Instance Ram running in Americas", "RAM", "OnDemand", "GiBy.h", 0.004237, "us-central1"),
		gcpSkuFixture("Nvidia Tesla A100 GPU running in Americas", "GPU", "OnDemand", "h", 2.933908, "us-central1"),
		gcpSkuFixture("Nvidia Tesla A100 GPU attached to Spot Preemptible VMs running in Americas", "GPU", "Preemptible", "h", 1.6, "us-central1"),
		// Two SKUs for one family and region: the lower price is kept.
		gcpSkuFixture("Compute optimized Core running in Americas", "CPU", "OnDemand", "h", 0.034, "us-central1"),
		gcpSkuFixture("Compute optimized Instance Core running in Virginia", "CPU", "OnDemand", "h", 0.033982, "us-central1"),
		gcpSkuFixture("Compute optimized Ram running in Americas", "RAM", "OnDemand", "GiBy.h", 0.004555, "us-central1"),
	}
}

func gcpMachines() []map[string]any {
	mt := func(name, zone string, cpus, memMb int, extra map[string]any) map[string]any {
		m := map[string]any{"name": name, "zone": zone, "guestCpus": cpus, "memoryMb": memMb, "description": name, "isSharedCpu": false}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	return []map[string]any{
		mt("n2-standard-2", "us-central1-a", 2, 8192, nil),
		mt("n2-standard-2", "us-central1-b", 2, 8192, nil),
		mt("n2-standard-2", "us-east1-b", 2, 8192, nil),
		mt("n2-standard-2", "europe-west1-b", 2, 8192, nil), // no N2 price there
		mt("c4d-standard-2", "us-central1-a", 2, 7168, map[string]any{"architecture": "X86_64"}),
		mt("e2-micro", "us-central1-a", 2, 1024, map[string]any{"isSharedCpu": true}),
		mt("f1-micro", "us-central1-a", 1, 614, map[string]any{"isSharedCpu": true}),
		mt("a2-highgpu-1g", "us-central1-a", 12, 87040, map[string]any{"accelerators": []map[string]any{{"guestAcceleratorType": "nvidia-tesla-a100", "guestAcceleratorCount": 1}}}),
		mt("c2-standard-4", "us-central1-a", 4, 16384, map[string]any{"deprecated": map[string]any{"state": "DEPRECATED"}}),
		mt("n1-standard-1", "us-central1-a", 1, 3840, nil), // only an N1 commitment is listed
		mt("ct5lp-hightpu-1t", "us-central1-a", 24, 49152, nil),
	}
}

func gcpFile(t *testing.T, fetched string, skus []gcpSku) *snapshot {
	t.Helper()
	data, _ := json.Marshal(gcpSource{Kind: gcpSourceKind, FetchedAt: fetched, Skus: skus, MachineTypes: gcpMachines()})
	if sourceKind(data) != gcpSourceKind {
		t.Fatal("fixture is not recognized as a GCP file")
	}
	s, err := readGCP(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGCP(t *testing.T) {
	s1 := gcpFile(t, "2026-11-01T00:00:00Z", gcpSkus(0.031611))
	std, spot := Variant{gcpStandard}, Variant{gcpSpot}
	od := func(name, region string, v Variant) float64 { return s1.Instances[name].OD[odKey{region, v}].Price }
	for _, c := range []struct {
		name, region string
		v            Variant
		want         float64
	}{
		{"n2-standard-2", "us-central1", std, 0.097118},
		{"n2-standard-2", "us-east1", std, 0.097118},
		{"n2-standard-2", "us-central1", spot, 0.058256},
		{"e2-micro", "us-central1", std, 0.00837643},
		{"f1-micro", "us-central1", std, 0.0076},
		{"a2-highgpu-1g", "us-central1", std, 3.673385},
		{"a2-highgpu-1g", "us-central1", spot, 0},       // A2 vCPUs have no Spot price in the fixture
		{"c2-standard-4", "us-central1", std, 0.208808}, // 4 × 0.033982 + 16 × 0.004555
		{"c4d-standard-2", "us-central1", std, roundPrice(2*0.03 + 7*0.001*1.073741824)},
	} {
		if got := od(c.name, c.region, c.v); got != c.want {
			t.Errorf("%s in %s %v = %v, want %v", c.name, c.region, c.v, got, c.want)
		}
	}
	n2 := s1.Instances["n2-standard-2"]
	if p := n2.RS[rsKey{"us-central1", std, Term{"1yr", "Committed Use", "No Upfront"}}]; p.Hourly != roundPrice(2*0.019915+8*0.002669) {
		t.Errorf("N2 commitment %v", p)
	}
	if len(n2.OD) != 3 {
		t.Errorf("n2-standard-2 should have no price in europe-west1: %v", n2.OD)
	}
	if _, ok := n2.Attrs["europe-west1"]; ok {
		t.Error("n2-standard-2 should have no attributes where it has no price")
	}
	if z := n2.Attrs["us-central1"]["zones"]; z != "us-central1-a,us-central1-b" {
		t.Errorf("zones %q", z)
	}
	if a := s1.Instances["a2-highgpu-1g"].Attrs["us-central1"]; a["acceleratorType"] != "nvidia-tesla-a100" || a["acceleratorCount"] != "1" || a["guestCpus"] != "12" {
		t.Errorf("A2 attributes %v", a)
	}
	if s1.Instances["c2-standard-4"].Attrs["us-central1"]["deprecated"] != "DEPRECATED" {
		t.Error("deprecated state")
	}
	if len(s1.Instances["e2-micro"].RS) != 0 {
		t.Error("shared-core machine types have no commitments")
	}
	if n1 := s1.Instances["n1-standard-1"]; n1 != nil {
		t.Errorf("n1-standard-1 has only a commitment vCPU price, no memory: %v", n1.RS)
	}
	if s1.Unpriced != 2 || s1.Collisions != 1 || s1.Products != 8 {
		t.Errorf("%d unpriced, %d collisions, %d products", s1.Unpriced, s1.Collisions, s1.Products)
	}

	b := &Bundle{Instances: map[string]*Instance{}}
	appendOK(t, b, s1)
	if _, err := b.Append(gcpFile(t, "2026-11-01T01:00:00Z", gcpSkus(0.031611))); !errors.Is(err, errUnchanged) {
		t.Fatalf("unchanged fetch: %v", err)
	}
	st := appendOK(t, b, gcpFile(t, "2026-11-01T02:00:00Z", gcpSkus(0.03)))
	if st.ODRows != 2 {
		t.Errorf("an N2 vCPU price change should change n2-standard-2 in two regions: %+v", st)
	}
}

func TestGCPMachineStems(t *testing.T) {
	for name, want := range map[string]string{
		"n2-standard-2": "n2", "c2-standard-60": "computeoptimized", "m1-megamem-96": "memory-optimized",
		"m3-ultramem-32": "m3memory-optimized", "a3-megagpu-8g": "a3plus", "a3-edgegpu-8g": "a3", "m4-ultramem-224": "m4ultramem224",
		"m4-ultramem-56": "m4", "z4d-highmem-16-highlssd": "z4d-highmem-highlssd", "h4d-megamem-192-lssd": "h4d-megamem-192",
		"a4-highgpu-8g": "-",
	} {
		if got := gcpMachineStem(name); got != want {
			t.Errorf("gcpMachineStem(%q) = %q, want %q", name, got, want)
		}
	}
	for desc, want := range map[string]string{"C4A Arm Instance": "c4a", "N1 Predefined Instance": "n1", "M4NUltramem224Instance": "m4nultramem224",
		"Compute optimized": "computeoptimized", "N2D AMD Instance": "n2d"} {
		if got := gcpStem(desc); got != want {
			t.Errorf("gcpStem(%q) = %q, want %q", desc, got, want)
		}
	}
}

func TestGCPFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/skus") && r.URL.Query().Get("pageToken") == "":
			fmt.Fprint(w, `{"skus":[{"description":"E2 Instance Core running in Americas","category":{"resourceFamily":"Compute","resourceGroup":"CPU","usageType":"OnDemand"},
				"serviceRegions":["us-central1"],"pricingInfo":[{"pricingExpression":{"usageUnit":"h","tieredRates":[{"unitPrice":{"currencyCode":"USD","units":"0","nanos":21811590}}]}}]},
				{"description":"Storage","category":{"resourceFamily":"Storage"}}],"nextPageToken":"p2"}`)
		case strings.HasSuffix(r.URL.Path, "/skus"):
			fmt.Fprint(w, `{"skus":[{"description":"E2 Instance Ram running in Americas","category":{"resourceFamily":"Compute","resourceGroup":"RAM","usageType":"OnDemand"},
				"serviceRegions":["us-central1"],"pricingInfo":[{"pricingExpression":{"usageUnit":"GiBy.h","tieredRates":[{"unitPrice":{"currencyCode":"USD","units":"0","nanos":2923530}}]}}]}]}`)
		case r.URL.Path == "/projects/proj/aggregated/machineTypes":
			fmt.Fprint(w, `{"items":{"zones/us-central1-a":{"machineTypes":[{"kind":"compute#machineType","id":"1","name":"e2-standard-2","guestCpus":2,"memoryMb":8192,
				"zone":"https://www.googleapis.com/compute/v1/projects/proj/zones/us-central1-a","selfLink":"x"}]},"zones/us-west9-z":{"warning":{"code":"NO_RESULTS_ON_PAGE"}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	src, err := fetchGCP(context.Background(), srv.Client(), srv.URL+"/skus", srv.URL+"/projects", gcpCreds{Token: "tok", Project: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Skus) != 2 || len(src.MachineTypes) != 1 || src.MachineTypes[0]["zone"] != "us-central1-a" || src.MachineTypes[0]["selfLink"] != nil {
		t.Fatalf("source %+v", src)
	}
	data, _ := json.Marshal(src)
	s, err := readGCP(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if p := s.Instances["e2-standard-2"].OD[odKey{"us-central1", Variant{gcpStandard}}].Price; p != 0.06701142 {
		t.Errorf("e2-standard-2 %v", p)
	}
	if _, err := fetchGCP(context.Background(), srv.Client(), srv.URL+"/skus", srv.URL+"/projects", gcpCreds{Token: "bad", Project: "proj"}); err == nil {
		t.Error("bad token should fail")
	}
}

func TestGCPServiceAccount(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:jwt-bearer":
			parts := strings.Split(r.Form.Get("assertion"), ".")
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var c map[string]any
			json.Unmarshal(claims, &c)
			if c["iss"] != "sa@proj.iam.gserviceaccount.com" || c["aud"] != "http://"+r.Host+"/token" || c["scope"] != gcpScopes {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"access_token":"sa-token"}`)
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt" {
				http.Error(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`, http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"access_token":"user-token"}`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	t.Setenv("GCP_ACCESS_TOKEN", "")
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	sa, _ := json.Marshal(gcpKeyFile{Type: "service_account", ProjectID: "proj", PrivateKey: pemKey,
		ClientEmail: "sa@proj.iam.gserviceaccount.com", TokenURI: srv.URL + "/token"})
	t.Setenv("GCP_SERVICE_ACCOUNT_KEY", string(sa))
	creds, err := gcpCredentials(ctx, srv.Client())
	if err != nil || creds != (gcpCreds{Token: "sa-token", Project: "proj"}) {
		t.Fatalf("service account: %+v %v", creds, err)
	}

	t.Setenv("GCP_SERVICE_ACCOUNT_KEY", "")
	user, _ := json.Marshal(gcpKeyFile{Type: "authorized_user", ClientID: "c", ClientSecret: "s", RefreshToken: "rt", QuotaProjectID: "quota", TokenURI: srv.URL + "/token"})
	path := filepath.Join(t.TempDir(), "adc.json")
	must(t, os.WriteFile(path, user, 0o600))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	t.Setenv("GCP_PROJECT", "other")
	if creds, err := gcpCredentials(ctx, srv.Client()); err != nil || creds != (gcpCreds{Token: "user-token", Project: "other"}) {
		t.Fatalf("authorized user: %+v %v", creds, err)
	}

	expired, _ := json.Marshal(gcpKeyFile{Type: "authorized_user", RefreshToken: "old", QuotaProjectID: "q", TokenURI: srv.URL + "/token"})
	must(t, os.WriteFile(path, expired, 0o600))
	if _, err := gcpCredentials(ctx, srv.Client()); err == nil || !strings.Contains(err.Error(), "expired or revoked") {
		t.Errorf("expired refresh token: %v", err)
	}

	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("GCP_ACCESS_TOKEN", "abc")
	t.Setenv("GCP_PROJECT", "")
	if _, err := gcpCredentials(ctx, srv.Client()); err == nil {
		t.Error("GCP_ACCESS_TOKEN without GCP_PROJECT should fail")
	}
}
