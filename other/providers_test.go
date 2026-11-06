package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readFixture encodes a source the way fetch writes it, checks that compress and append would recognize its
// kind, and reads it back.
func readFixture[S any](t *testing.T, src S, kind string, read func(io.Reader) (*snapshot, error)) *snapshot {
	t.Helper()
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceKind(data); got != kind {
		t.Fatalf("source kind: got %q, want %q", got, kind)
	}
	s, err := read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newBundle() *Bundle { return &Bundle{Instances: map[string]*Instance{}} }

func wantOD(t *testing.T, s *snapshot, name, region string, variant Variant, want float64) {
	t.Helper()
	si := s.Instances[name]
	if si == nil {
		t.Fatalf("%s is missing", name)
	}
	got, ok := si.OD[odKey{Region: region, Variant: variant}]
	if !ok {
		t.Fatalf("%s has no price in %s for %v; has %v", name, region, variant, si.OD)
	}
	if got.Price != want {
		t.Errorf("%s in %s %v: got %v, want %v", name, region, variant, got.Price, want)
	}
}

func wantRS(t *testing.T, s *snapshot, name, region string, variant Variant, term Term, hourly, upfront float64) {
	t.Helper()
	got, ok := s.Instances[name].RS[rsKey{Region: region, Variant: variant, Term: term}]
	if !ok {
		t.Fatalf("%s has no %v in %s for %v; has %v", name, term, region, variant, s.Instances[name].RS)
	}
	if got.Hourly != hourly || got.Upfront != upfront {
		t.Errorf("%s %v in %s: got %v/%v, want %v/%v", name, term, region, got.Hourly, got.Upfront, hourly, upfront)
	}
}

// --- Hetzner ---

func hetznerRowHTML(typ, name, cpu, ram, disk, key string) string {
	return fmt.Sprintf(`<div
    class="cloud-matrix-product "
    style="order: 1;"
>
  <div class="cell name-cell"><span>%s</span></div>
  <div class="cell cpu-cell"><svg><path d="M1 2 3"/></svg> %s</div>
  <div class="cell ram-cell">%s</div>
  <div class="cell drive-cell"><svg></svg> %s <div><span>NVMe</span></div></div>
  <div class="cell month-price-cell"><ho-price-container product-key="%s"></ho-price-container></div>
  <div class="cell btn-cell"><a href="https://console.hetzner.com/create/server?type=%s&amp;useIPv4=true">Create</a></div>
</div>`, name, cpu, ram, disk, key, typ)
}

func TestParseHetznerPage(t *testing.T) {
	page := `<div class="other"></div>` +
		hetznerRowHTML("cx23", "CX23 not available", "2 Intel®/AMD", "4 GB", "40 GB", "CLOUD_132+CLOUD_21") +
		hetznerRowHTML("cax11", "CAX11", "2 Ampere®", "4 GB", "40 GB", "CLOUD_111+CLOUD_21") +
		// The same server again, for another screen size.
		hetznerRowHTML("cax11", "CAX11", "2 Ampere®", "4 GB", "40 GB", "CLOUD_111+CLOUD_21")
	servers, err := parseHetznerPage(page, "Cost-Optimized")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("got %d servers: %+v", len(servers), servers)
	}
	cx := servers[0]
	if cx.Name != "CX23" || cx.Cores != "2" || cx.CPU != "Intel®/AMD" || cx.Memory != "4" || cx.Disk != "40" || cx.DiskType != "NVMe" {
		t.Errorf("cx23: %+v", cx)
	}
	if strings.Join(cx.Products, "+") != "CLOUD_132+CLOUD_21" {
		t.Errorf("cx23 products: %v", cx.Products)
	}

	// A page whose rows can't be read must fail, or every server would be deleted from the bundle.
	if _, err := parseHetznerPage(`<p>we redesigned</p>`, "x"); err == nil {
		t.Error("a page with no servers should be an error")
	}
	broken := hetznerRowHTML("cx23", "CX23", "2 AMD", "lots", "40 GB", "CLOUD_132+CLOUD_21")
	if _, err := parseHetznerPage(broken, "x"); err == nil {
		t.Error("a row with an unreadable cell should be an error")
	}
}

func hetznerFixture(fetched, hourly string) hetznerSource {
	price := func(h, m string) map[string]map[string]string {
		return map[string]map[string]string{"hourly": {"USD": h, "EUR": "0.5"}, "monthly": {"USD": m, "EUR": "1"}}
	}
	return hetznerSource{Kind: hetznerSourceKind, FetchedAt: fetched,
		Servers: []hetznerServer{{Type: "cax11", Name: "CAX11", Category: "Cost-Optimized", Cores: "2", CPU: "Ampere®", Memory: "4", Disk: "40", DiskType: "NVMe",
			Products: []string{"CLOUD_111", "CLOUD_21"}}},
		Products: map[string]hetznerProduct{
			"CLOUD_21": {ProductID: "CLOUD_21", Locations: []hetznerLocation{{Active: true, Datacenter: "ALL", Prices: price("0.001", "0.60")}}},
			"CLOUD_111": {ProductID: "CLOUD_111", Locations: []hetznerLocation{
				{CountryCode: "de", Active: true, Datacenter: "FSN1", Prices: price(hourly, "6.99")},
				{CountryCode: "us", Active: true, Datacenter: "ASH1", Prices: price("0.0200", "12.49")},
				{CountryCode: "fi", Active: false, Datacenter: "HEL1", Prices: price("0.9", "9")},
			}},
		}}
}

func TestHetzner(t *testing.T) {
	s := readFixture(t, hetznerFixture("2026-01-01T00:00:00Z", "0.0112"), hetznerSourceKind, readHetzner)
	if s.Offer != hetznerOffer {
		t.Errorf("offer: %s", s.Offer)
	}
	ipv6, ipv4 := Variant{hetznerIPv6Only}, Variant{hetznerWithIPv4}
	wantOD(t, s, "cax11", "fsn1", ipv6, 0.0112)
	wantOD(t, s, "cax11", "fsn1", ipv4, 0.0122)
	wantOD(t, s, "cax11", "ash", ipv6, 0.02)
	wantRS(t, s, "cax11", "fsn1", ipv4, hetznerMonthlyTerm, 0, 7.59)
	if _, sold := s.Instances["cax11"].OD[odKey{Region: "hel1", Variant: ipv6}]; sold {
		t.Error("a location that isn't active should have no price")
	}
	if got := s.Instances["cax11"].Attrs["fsn1"]["architecture"]; got != "arm64" {
		t.Errorf("architecture: %q", got)
	}
	if s.Regions["ash"]["name"] != "Ashburn, VA" || s.Regions["ash"]["country"] != "us" {
		t.Errorf("regions: %v", s.Regions["ash"])
	}

	b := newBundle()
	appendOK(t, b, s)
	if _, err := b.Append(readFixture(t, hetznerFixture("2026-01-02T00:00:00Z", "0.0112"), hetznerSourceKind, readHetzner)); !errors.Is(err, errUnchanged) {
		t.Errorf("an unchanged read: got %v", err)
	}
	appendOK(t, b, readFixture(t, hetznerFixture("2026-01-03T00:00:00Z", "0.0120"), hetznerSourceKind, readHetzner))
}

func TestHetznerFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloud/cost-optimized/":
			fmt.Fprint(w, hetznerRowHTML("cax11", "CAX11", "2 Ampere®", "4 GB", "40 GB", "CLOUD_111+CLOUD_21"))
		case "/cloud/regular-performance/":
			fmt.Fprint(w, hetznerRowHTML("cpx22", "CPX22", "2 AMD", "4 GB", "80 GB", "CLOUD_124+CLOUD_21"))
		case "/cloud/general-purpose/":
			fmt.Fprint(w, hetznerRowHTML("ccx13", "CCX13", "2 AMD", "8 GB", "80 GB", "CLOUD_115+CLOUD_21"))
		default:
			id := strings.TrimPrefix(r.URL.Path, "/products/")
			fmt.Fprintf(w, `{"productId":%q,"locations":[{"active":true,"datacenter":"FSN1","prices":{"hourly":{"USD":"0.01"},"monthly":{"USD":"6"}}}]}`, id)
		}
	}))
	defer srv.Close()
	src, err := fetchHetzner(context.Background(), srv.Client(), srv.URL, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Servers) != 3 || len(src.Products) != 4 {
		t.Errorf("got %d servers and %d products", len(src.Servers), len(src.Products))
	}
	s := readFixture(t, *src, hetznerSourceKind, readHetzner)
	wantOD(t, s, "ccx13", "fsn1", Variant{hetznerIPv6Only}, 0.01)
}

func auctionFixture(fetched string, ids ...int) hetznerAuctionSource {
	src := hetznerAuctionSource{Kind: hetznerAuctionSourceKind, FetchedAt: fetched}
	for i, id := range ids {
		var server map[string]any
		raw := fmt.Sprintf(`{"Id":%d,
			"Hardware":{"CPU":{"Name":"Intel Core i7-7700","CoreCount":1},"RAM":{"Size":64,"SizeUnit":"GB","ecc":false}},
			"Prices":{"monthly":{"EUR":76,"USD":%d},"hourly":{"EUR":0.12,"USD":0.1362},"setup":{"EUR":0,"USD":0},"fixed":false},
			"IPPrices":{"monthly":{"EUR":1.7,"USD":1.9},"hourly":{"EUR":0.0027,"USD":0.003},"Amount":1},
			"Details":{"Specials":["IPv4","iNIC"],"Traffic":"unlimited","Datacenter":{"Name":"FSN1-DC4","Datacenter":"#FSN1-DC4"}},
			"Timer":{"ReduceNext":71371}}`, id, 85+i)
		if err := decodeNumbers([]byte(raw), &server); err != nil {
			panic(err)
		}
		src.Servers = append(src.Servers, server)
	}
	return src
}

func TestHetznerAuction(t *testing.T) {
	s := readFixture(t, auctionFixture("2026-01-01T00:00:00Z", 3107671, 3107672), hetznerAuctionSourceKind, readHetznerAuction)
	ipv4 := Variant{hetznerWithIPv4}
	wantOD(t, s, "3107671", "fsn1", ipv4, 0.1392)
	wantRS(t, s, "3107671", "fsn1", ipv4, hetznerMonthlyTerm, 0, 86.9)
	wantRS(t, s, "3107671", "fsn1", Variant{hetznerIPv6Only}, hetznerMonthlyTerm, 0, 85)
	attrs := s.Instances["3107671"].Attrs["fsn1"]
	if attrs["CPU.Name"] != "Intel Core i7-7700" || attrs["datacenter"] != "FSN1-DC4" || attrs["fixedPrice"] != "false" || attrs["Specials"] != "IPv4,iNIC" {
		t.Errorf("attributes: %v", attrs)
	}
	if _, timer := attrs["Timer.ReduceNext"]; timer {
		t.Error("the auction's countdown would record a change on every read")
	}

	// A sold server is gone from the feed, and its history ends.
	b := newBundle()
	appendOK(t, b, s)
	st := appendOK(t, b, readFixture(t, auctionFixture("2026-01-02T00:00:00Z", 3107672), hetznerAuctionSourceKind, readHetznerAuction))
	if st.Deleted != 1 || b.Instances["3107671"].DeletedAt == nil {
		t.Errorf("the sold server should be deleted: %+v", st)
	}
}

// --- OVHcloud Public Cloud ---

func ovhAddon(code string, price float64, unit string, tech map[string]any) ovhFlavor {
	interval := 1
	if unit == "none" {
		interval = 0
	}
	a := ovhFlavor{PlanCode: code, Product: ovhFlavorProduct, Pricings: []ovhPricing{{Capacities: []string{"consumption"}, Interval: interval, IntervalUnit: unit, Price: price * ovhPriceScale, Mode: "default"}}}
	if tech != nil {
		a.Blobs = map[string]any{"technical": tech, "commercial": map[string]any{"brick": "guaranteed-resources"}}
	}
	return a
}

func TestOVH(t *testing.T) {
	linux := map[string]any{"name": "b2-7", "memory": map[string]any{"size": 7}, "os": map[string]any{"family": "linux"}}
	windows := map[string]any{"name": "win-b2-7", "memory": map[string]any{"size": 70}, "os": map[string]any{"family": "windows"}}
	src := ovhSource{Kind: ovhSourceKind, FetchedAt: "2026-01-01T00:00:00Z", Catalogs: []ovhCatalog{
		{Locale: ovhLocale{CurrencyCode: "USD", Subsidiary: "WS"}, Addons: []ovhFlavor{
			ovhAddon("win-b2-7.consumption", 0.2251, "hour", windows),
			ovhAddon("b2-7.consumption", 0.0837, "hour", linux),
			ovhAddon("b2-7.consumption.3AZ", 0.09, "hour", linux),
			ovhAddon("b2-7.consumption.LZ.AF", 0.105, "hour", linux),
			ovhAddon("b2-7.monthly.postpaid", 50.5, "month", linux),
			ovhAddon("metal.eg-32.consumption", 1.5, "hour", nil),
			ovhAddon("oddity", 1, "hour", nil),
		}},
		{Locale: ovhLocale{CurrencyCode: "USD", Subsidiary: "US"}, Addons: []ovhFlavor{
			ovhAddon("b2-7.consumption", 0.0879, "none", linux),
			ovhAddon("b2-7.consumption.US-EAST-LZ-DAL-A", 0.1, "none", linux),
			ovhAddon("b2-7.monthly", 52, "month", linux),
			ovhAddon("b2-7.monthly.postpaid", 55, "none", linux),
		}},
	}}
	s := readFixture(t, src, ovhSourceKind, readOVH)
	lin, win := Variant{"Linux"}, Variant{"Windows"}
	wantOD(t, s, "b2-7", "world", lin, 0.0837)
	wantOD(t, s, "b2-7", "world", win, 0.2251)
	wantOD(t, s, "b2-7", "3az", lin, 0.09)
	wantOD(t, s, "b2-7", "lz-af", lin, 0.105)
	wantOD(t, s, "b2-7", "us", lin, 0.0879)
	wantOD(t, s, "b2-7", "us-east-lz-dal-a", lin, 0.1)
	wantOD(t, s, "metal.eg-32", "world", lin, 1.5)
	wantRS(t, s, "b2-7", "world", lin, Term{"1mo", "Monthly", "Postpaid"}, 0, 50.5)
	wantRS(t, s, "b2-7", "us", lin, Term{"1mo", "Monthly", "All Upfront"}, 0, 52)
	wantRS(t, s, "b2-7", "us", lin, Term{"1mo", "Monthly", "Postpaid"}, 0, 55)
	if s.Skipped != 1 {
		t.Errorf("skipped: %d", s.Skipped)
	}
	attrs := s.Instances["b2-7"].Attrs[""]
	if attrs["memory.size"] != "7" || attrs["brick"] != "guaranteed-resources" {
		t.Errorf("Linux specs should win over Windows ones: %v", attrs)
	}
	if _, ok := attrs["os.family"]; ok {
		t.Error("the operating system belongs to the variant")
	}
	appendOK(t, newBundle(), s)

	src.Catalogs[0].Locale.CurrencyCode = "EUR"
	data, _ := json.Marshal(src)
	if _, err := readOVH(bytes.NewReader(data)); err == nil {
		t.Error("a catalog that isn't in USD should be an error")
	}
}

// --- OVHcloud eco ---

func ecoPricings(setup, monthly, year float64) []ovhPricing {
	return []ovhPricing{
		{Capacities: []string{"installation"}, IntervalUnit: "none", Price: setup * ovhPriceScale, Mode: "default"},
		{Capacities: []string{"renew"}, Interval: 1, IntervalUnit: "month", Price: monthly * ovhPriceScale, Mode: "default"},
		{Capacities: []string{"installation"}, IntervalUnit: "none", Mode: "upfront12"},
		{Capacities: []string{"renew"}, Interval: 12, IntervalUnit: "month", Commitment: 12, Price: year * ovhPriceScale, Mode: "upfront12"},
	}
}

func TestOVHEco(t *testing.T) {
	families := []ovhEcoFamily{
		{Name: "memory", Mandatory: true, Default: "ram-32g-24sys01", Addons: []string{"ram-32g-24sys01", "ram-64g-24sys01"}},
		{Name: "storage", Mandatory: true, Default: "softraid-2x4000sa-24sys01", Addons: []string{"softraid-2x4000sa-24sys01"}},
		{Name: "bandwidth", Mandatory: true, Default: "bandwidth-1000", Addons: []string{"bandwidth-1000"}},
		{Name: "application-license", Addons: []string{"cpanel"}},
	}
	plan := func(code string, base float64, dcs ...string) ovhEcoPlan {
		return ovhEcoPlan{PlanCode: code, InvoiceName: "SYS-1 | Intel Xeon-E 2136", Product: "24sys01", Pricings: ecoPricings(33.2, base, base*11.4),
			AddonFamilies: families, Configurations: []ovhEcoConfig{{Name: "dedicated_datacenter", Values: dcs}, {Name: "region", Values: []string{"europe"}}}}
	}
	src := ovhEcoSource{Kind: ovhEcoSourceKind, FetchedAt: "2026-01-01T00:00:00Z", Locale: ovhLocale{CurrencyCode: "USD", Subsidiary: "WS"},
		Plans: []ovhEcoPlan{plan("24sys01-v1", 33.2, "gra", "rbx"), plan("24sys01-v1-sgp", 40, "sgp")},
		Addons: []ovhEcoAddon{
			{PlanCode: "ram-32g-24sys01", Pricings: ecoPricings(0, 0, 0)},
			{PlanCode: "ram-64g-24sys01", Pricings: ecoPricings(0, 11.1, 126.54)},
			{PlanCode: "softraid-2x4000sa-24sys01", Pricings: ecoPricings(0, 0, 0)},
			{PlanCode: "bandwidth-1000", Pricings: ecoPricings(0, 1, 11)},
		},
		Products: []ovhEcoProduct{{Name: "24sys01", Blobs: map[string]any{"technical": map[string]any{"server": map[string]any{"range": "soyoustart", "cpu": map[string]any{"cores": 6}}}}}},
		Options: []ovhEcoOption{
			{PlanCode: "24sys01-v1", Memory: "ram-32g", Storage: "softraid-2x4000sa"},
			{PlanCode: "24sys01-v1", Memory: "ram-64g", Storage: "softraid-2x4000sa"},
			{PlanCode: "24sys01-v1-sgp", Memory: "ram-32g", Storage: "softraid-2x4000sa"},
			// The list of what's available can name a size the catalog has no price for.
			{PlanCode: "24sys01-v1", Memory: "ram-128g", Storage: "softraid-2x4000sa"},
		}}
	s := readFixture(t, src, ovhEcoSourceKind, readOVHEco)
	small, big := Variant{"ram-32g", "softraid-2x4000sa", ""}, Variant{"ram-64g", "softraid-2x4000sa", ""}
	// The rent is 33.2 plus the mandatory bandwidth's 1.
	rent := Term{"1mo", "Rental", "All Upfront"}
	wantRS(t, s, "24sys01", "gra", small, rent, 0, 34.2)
	wantRS(t, s, "24sys01", "rbx", big, rent, 0, 45.3)
	wantRS(t, s, "24sys01", "sgp", small, rent, 0, 41)
	if od := s.Instances["24sys01"].OD; len(od) != 0 {
		t.Errorf("servers rented by the month should have no hourly price: %v", od)
	}
	wantRS(t, s, "24sys01", "gra", big, Term{"1yr", "Prepaid", "All Upfront"}, 0, roundPrice(33.2*11.4+126.54+11))
	if s.Skipped != 1 {
		t.Errorf("skipped: %d", s.Skipped)
	}
	attrs := s.Instances["24sys01"].Attrs["sgp"]
	if attrs["range"] != "soyoustart" || attrs["cpu.cores"] != "6" || attrs["billing"] != "Monthly" || attrs["setupFee"] != "33.2" || attrs["planCode"] != "24sys01-v1-sgp" {
		t.Errorf("attributes: %v", attrs)
	}
	if s.Regions["gra"]["name"] != "Gravelines" {
		t.Errorf("regions: %v", s.Regions["gra"])
	}
	appendOK(t, newBundle(), s)
}

// --- Oracle ---

func oracleFixturePart(number, name, metric string, prices ...float64) oraclePart {
	p := oraclePart{PartNumber: number, DisplayName: name, MetricName: metric}
	loc := struct {
		CurrencyCode string `json:"currencyCode"`
		Prices       []struct {
			Model string  `json:"model"`
			Value float64 `json:"value"`
		} `json:"prices"`
	}{CurrencyCode: "USD"}
	for _, v := range prices {
		loc.Prices = append(loc.Prices, struct {
			Model string  `json:"model"`
			Value float64 `json:"value"`
		}{"PAY_AS_YOU_GO", v})
	}
	p.CurrencyCodeLocalizations = append(p.CurrencyCodeLocalizations, loc)
	return p
}

func qty(v float64) *float64 { return &v }

func oracleFixture() oracleSource {
	part := func(typ, number string, q float64, max *float64) oracleShapePart {
		return oracleShapePart{Type: oracleLabel{typ}, PartNumber: number, Qty: q, Max: max}
	}
	return oracleSource{Kind: oracleSourceKind, FetchedAt: "2026-01-01T00:00:00Z",
		Parts: []oraclePart{
			oracleFixturePart("E5O", "Compute - Standard - E5 - OCPU", "OCPU Per Hour", 0.03),
			oracleFixturePart("E5M", "Compute - Standard - E5 - Memory", "Gigabytes Per Hour", 0.002),
			oracleFixturePart("A1O", "Compute - Standard - A1 - OCPU", "OCPU Per Hour", 0, 0.01),
			oracleFixturePart("A1M", "Compute - Standard - A1 - Memory", "Gigabyte Per Hour", 0, 0.0015),
			oracleFixturePart("H100", "Compute - GPU - H100", "GPU Per Hour", 10),
			oracleFixturePart("DO", "Dense I/O - E5 OCPU", "OCPU Per Hour", 0.03),
			oracleFixturePart("DM", "Dense I/O - E5 Memory", "Gigabytes Per Hour", 0.002),
			oracleFixturePart("DN", "Dense I/O - E5 NVMe", "NVMe Terabyte Per Hour", 0.0612),
			oracleFixturePart("WIN", oracleWindowsPart, "OCPU Per Hour", 0.092),
		},
		Shapes: []oracleShape{
			{Name: "VM.Standard.E5.Flex", Status: "ACTIVE", SubType: oracleLabel{"flexible"}, ShapeType: oracleLabel{"vm"}, WindowsSupported: true,
				Products: []oracleShapePart{part("ocpu", "E5O", 0, qty(5)), part("memory", "E5M", 0, qty(40))}},
			{Name: "VM.Standard.A1.Flex", Status: "ACTIVE", SubType: oracleLabel{"flexible"}, ShapeType: oracleLabel{"vm"},
				Products: []oracleShapePart{part("ocpu", "A1O", 0, qty(2)), part("memory", "A1M", 0, qty(64))}},
			{Name: "BM.GPU.H100.8", Status: "ACTIVE", SubType: oracleLabel{"gpu"}, ShapeType: oracleLabel{"bm"}, BundleMemoryQty: 2048, GpuQty: qty(8), GpuMemoryQty: qty(640),
				Products: []oracleShapePart{part("ocpu", "H100", 112, nil)}},
			{Name: "VM.DenseIO.E5.Flex", Status: "ACTIVE", SubType: oracleLabel{"flexible"}, ShapeType: oracleLabel{"vm"},
				Products: []oracleShapePart{part("ocpu", "DO", 0, qty(48)), part("memory", "DM", 0, qty(576)), part("disk", "DN", 0, qty(40.8))}},
			{Name: "BM.Retired.1", Status: "INACTIVE", SubType: oracleLabel{"standard"}, Products: []oracleShapePart{part("ocpu", "E5O", 1, nil)}},
			{Name: "VM.NoPrice.1", Status: "ACTIVE", SubType: oracleLabel{"standard"}, Products: []oracleShapePart{part("ocpu", "NOPE", 1, nil)}},
		}}
}

func TestOracle(t *testing.T) {
	s := readFixture(t, oracleFixture(), oracleSourceKind, readOracle)
	lin, win := Variant{"Linux"}, Variant{"Windows"}
	// Flexible shapes are priced at powers of two OCPUs and the maximum, at each memory ratio within the maximum.
	wantOD(t, s, "VM.Standard.E5.Flex-1-16", oracleRegion, lin, 0.062)
	wantOD(t, s, "VM.Standard.E5.Flex-1-16", oracleRegion, win, 0.154)
	wantOD(t, s, "VM.Standard.E5.Flex-5-40", oracleRegion, lin, 0.23)
	if s.Instances["VM.Standard.E5.Flex-4-64"] != nil {
		t.Error("a size over the shape's maximum memory should be left out")
	}
	if s.Instances["VM.Standard.E5.Flex"] != nil {
		t.Error("a flexible shape has no one price of its own")
	}
	// A free tier isn't the price: the paid rate is.
	wantOD(t, s, "VM.Standard.A1.Flex-2-16", oracleRegion, lin, 0.044)
	if _, ok := s.Instances["VM.Standard.A1.Flex-2-16"].OD[odKey{Region: oracleRegion, Variant: win}]; ok {
		t.Error("a shape without Windows support should have no Windows price")
	}
	// GPU parts are per GPU, whatever quantity the shape lists for them.
	wantOD(t, s, "BM.GPU.H100.8", oracleRegion, lin, 80)
	wantOD(t, s, "VM.DenseIO.E5.Flex-8-96", oracleRegion, lin, 0.84816)
	wantOD(t, s, "VM.DenseIO.E5.Flex-48-576", oracleRegion, lin, 5.08896)
	if s.Instances["BM.Retired.1"] != nil || s.Instances["VM.NoPrice.1"] != nil {
		t.Error("inactive and unpriced shapes should be left out")
	}
	if s.Unpriced != 1 {
		t.Errorf("unpriced: %d", s.Unpriced)
	}
	attrs := s.Instances["VM.Standard.E5.Flex-1-16"].Attrs[""]
	if attrs["shape"] != "VM.Standard.E5.Flex" || attrs["ocpus"] != "1" || attrs["memoryInGb"] != "16" || attrs["ocpuPrice"] != "0.03" || attrs["memoryGbPrice"] != "0.002" {
		t.Errorf("attributes: %v", attrs)
	}
	gpu := s.Instances["BM.GPU.H100.8"].Attrs[""]
	if gpu["ocpus"] != "112" || gpu["memoryInGb"] != "2048" || gpu["gpuCount"] != "8" {
		t.Errorf("attributes: %v", gpu)
	}
	appendOK(t, newBundle(), s)
}

func TestOracleFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/parts":
			fmt.Fprint(w, `{"items":[{"partNumber":"E5O","displayName":"E5","metricName":"OCPU Per Hour","currencyCodeLocalizations":[{"currencyCode":"USD","prices":[{"model":"PAY_AS_YOU_GO","value":0.03}]}]}]}`)
		case "/presets":
			fmt.Fprint(w, `{"items":[
				{"presetShapes":[{"name":"BM.Standard.E5.192","status":"ACTIVE","products":[]}]},
				{"presetShapes":[
					{"name":"BM.Standard.E5.192","status":"ACTIVE","subType":{"value":"standard"},"products":[{"type":{"value":"ocpu"},"partNumber":"E5O","qty":192}]},
					{"name":"PostgreSQL.VM.Standard.E5.Flex","status":"ACTIVE","products":[]},
					{"name":"DVH.Standard.E5.192","status":"ACTIVE","products":[]}]},
				{"presetShapes":null}]}`)
		}
	}))
	defer srv.Close()
	src, err := fetchOracle(context.Background(), srv.Client(), srv.URL+"/parts", srv.URL+"/presets")
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Shapes) != 1 || len(src.Shapes[0].Products) != 1 {
		t.Fatalf("shapes: %+v", src.Shapes)
	}
	wantOD(t, readFixture(t, *src, oracleSourceKind, readOracle), "BM.Standard.E5.192", oracleRegion, Variant{"Linux"}, 5.76)
}

// --- IBM ---

func ibmFixture(fetched string, ownPrice float64) ibmSource {
	metric := func(id string, q, p float64) ibmMetric { return ibmMetric{ID: id, Quantity: q, Price: p} }
	return ibmSource{Kind: ibmSourceKind, FetchedAt: fetched,
		Profiles: []ibmProfile{
			{Name: "bx3d-2x10", DisplayName: "Balanced 2x10", Family: "balanced", Generation: "gt", Regions: []string{"us-south", "au-syd"},
				Config:      map[string]any{"cpu": json.Number("2"), "ram": json.Number("10"), "vcpu_manufacturer": "Intel"},
				Deployments: []ibmDeployment{{Type: "multi-tenant", Plan: "own"}, {Type: "dedicated", Plan: "own"}, {Type: "reservation-one-year", Plan: "rv-own"}}},
			{Name: "bx2-2x8", Regions: []string{"us-south"}, Deployments: []ibmDeployment{{Type: "multi-tenant", Plan: "shared"}, {Type: "reservation-one-year", Plan: "rv-shared"}}},
			{Name: "ux2d-2x56", Regions: []string{"us-south"}, Deployments: []ibmDeployment{{Type: "multi-tenant", Plan: "shared"}, {Type: "reservation-one-year", Plan: "private"}}},
			{Name: "bc1-2x8"}, // the oldest profiles have no plan
		},
		Plans: map[string][]ibmPlanRegion{
			"own": {
				{Region: "us-south", EffectiveFrom: "2025-11-01T00:00:00Z", Metrics: []ibmMetric{
					metric("part-is.instance-hours-bx3d-2x10", 1, ownPrice), metric("part-is.spot-bx3d-2x10", 1, 0.03), metric("part-is.burstable50-bx3d-2x10", 1, 0.07)}},
				{Region: "au-syd", EffectiveFrom: "2025-11-01T00:00:00Z", Metrics: []ibmMetric{metric("part-is.instance-hours-bx3d-2x10", 1, 0.138)}},
				// Sold by the plan, but not this profile.
				{Region: "eu-de", Metrics: []ibmMetric{metric("part-is.instance-hours-bx3d-2x10", 1, 0.5)}},
			},
			"rv-own": {{Region: "us-south", EffectiveFrom: "2025-11-01T00:00:00Z", Metrics: []ibmMetric{
				metric("part-is.reservation-hours-bx3d-2x10-hourly", 1, 0.113),
				metric("part-is.reservation-hours-bx3d-2x10-one-year", 1, 0.0737),
				metric("part-is.reservation-hours-bx3d-2x10-three-year", 1, 0.0451)}}},
			"shared": {{Region: "us-south", Metrics: []ibmMetric{metric("part-is.cpu-hours", 63, 1.7626), metric("part-is.ram-hours", 450, 2.0054)}}},
			"rv-shared": {{Region: "us-south", Metrics: []ibmMetric{
				metric("part-is.reservation-hours-bx2-2x8-hourly", 1, 0.101),
				metric("part-is.reservation-hours-bx2-2x8-one-year", 1, 0.0656)}}},
		},
		MissingPlans: []string{"private"}}
}

func TestIBM(t *testing.T) {
	s := readFixture(t, ibmFixture("2026-01-01T00:00:00Z", 0.113), ibmSourceKind, readIBM)
	od := Variant{ibmOnDemand}
	wantOD(t, s, "bx3d-2x10", "us-south", od, 0.113)
	wantOD(t, s, "bx3d-2x10", "au-syd", od, 0.138)
	wantOD(t, s, "bx3d-2x10", "us-south", Variant{"Spot"}, 0.03)
	wantOD(t, s, "bx3d-2x10", "us-south", Variant{"Burstable 50%"}, 0.07)
	if _, ok := s.Instances["bx3d-2x10"].OD[odKey{Region: "eu-de", Variant: od}]; ok {
		t.Error("a region the profile isn't sold in should have no price")
	}
	wantRS(t, s, "bx3d-2x10", "us-south", od, Term{"1yr", "Reserved", "No Upfront"}, 0.0737, 0)
	wantRS(t, s, "bx3d-2x10", "us-south", od, Term{"3yr", "Reserved", "No Upfront"}, 0.0451, 0)
	if _, hourly := s.Instances["bx3d-2x10"].RS[rsKey{Region: "us-south", Variant: od, Term: Term{"1hr", "Reserved", "No Upfront"}}]; hourly {
		t.Error("an hourly reservation isn't a term")
	}
	// A profile on a shared plan takes its own figure, not the plan's per-vCPU and per-GB rates, which come to 0.0916.
	wantOD(t, s, "bx2-2x8", "us-south", od, 0.101)
	wantRS(t, s, "bx2-2x8", "us-south", od, Term{"1yr", "Reserved", "No Upfront"}, 0.0656, 0)
	if s.Instances["ux2d-2x56"] != nil || s.Instances["bc1-2x8"] != nil {
		t.Error("a profile with no price of its own should be left out")
	}
	if s.Unpriced != 2 {
		t.Errorf("unpriced: %d", s.Unpriced)
	}
	if got := s.Instances["bx3d-2x10"].OD[odKey{Region: "us-south", Variant: od}].Eff; fmtTS(got) != "2025-11-01T00:00:00Z" {
		t.Errorf("a price keeps the date IBM gives it: %s", fmtTS(got))
	}
	attrs := s.Instances["bx3d-2x10"].Attrs[""]
	if attrs["cpu"] != "2" || attrs["displayName"] != "Balanced 2x10" || attrs["family"] != "balanced" {
		t.Errorf("attributes: %v", attrs)
	}
	if s.Regions["au-syd"]["name"] != "Sydney" {
		t.Errorf("regions: %v", s.Regions)
	}

	b := newBundle()
	appendOK(t, b, s)
	if _, err := b.Append(readFixture(t, ibmFixture("2026-01-02T00:00:00Z", 0.113), ibmSourceKind, readIBM)); !errors.Is(err, errUnchanged) {
		t.Errorf("an unchanged read: got %v", err)
	}
	appendOK(t, b, readFixture(t, ibmFixture("2026-01-03T00:00:00Z", 0.12), ibmSourceKind, readIBM))
}

func TestIBMFetch(t *testing.T) {
	deployment := func(location, origin string, price float64) string {
		return fmt.Sprintf(`{"deployment_location":%q,"deployment_region":"us-south","origin":%q,"effective_from":"2026-11-01T00:00:00Z","metrics":[
			{"metric_id":"part-is.instance-hours-bx3d-2x10","charge_unit_quantity":1,"amounts":[
				{"country":"EURO","prices":[{"price":0.09}]},{"country":"USA","prices":[{"price":%g}]}]},
			{"metric_id":"part-is.rhelOS","charge_unit_quantity":1,"amounts":[{"country":"USA","prices":[{"price":9}]}]}]}`, location, origin, price)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "":
			if r.URL.Query().Get("_offset") != "0" {
				fmt.Fprint(w, `{"count":1,"resources":[]}`)
				return
			}
			fmt.Fprint(w, `{"count":1,"resources":[{"name":"bx3d-2x10","overview_ui":{"en":{"display_name":"Balanced"}},"metadata":{"other":{
				"geography":{"regions":[{"name":"us-south"}]},
				"profile":{"family":"balanced","default_config":{"cpu":2,"bss_info":[1]},"measures":[
					{"component":"instance","deployments":[{"type":"multi-tenant","plan":"good"}]},
					{"component":"reservation","deployments":[{"type":"reservation-one-year","plan":"secret"}]}]}}}}]}`)
		case "/good/pricing/deployment":
			fmt.Fprintf(w, `{"count":3,"resources":[%s,%s,%s]}`,
				deployment("us-south", "pricing_catalog", 0.113),
				deployment("us-south-1", "pricing_catalog", 0.5), // a zone
				deployment("us-south", "global_catalog", 3.58))   // an old per-profile deployment
		case "/secret/pricing/deployment":
			http.Error(w, `{"message":"Not authenticated"}`, http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	src, err := fetchIBM(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.MissingPlans) != 1 || src.MissingPlans[0] != "secret" {
		t.Errorf("missing plans: %v", src.MissingPlans)
	}
	if got := src.Plans["good"]; len(got) != 1 || len(got[0].Metrics) != 1 || got[0].Metrics[0].Price != 0.113 {
		t.Errorf("plan prices: %+v", got)
	}
	if _, hidden := src.Profiles[0].Config["bss_info"]; hidden {
		t.Error("billing settings aren't specs")
	}
	wantOD(t, readFixture(t, *src, ibmSourceKind, readIBM), "bx3d-2x10", "us-south", Variant{ibmOnDemand}, 0.113)
}
