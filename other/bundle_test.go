package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// roundTrip writes and rereads a bundle, checking nothing is lost.
func roundTrip(t *testing.T, b *Bundle) *Bundle {
	t.Helper()
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBundle(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, b) {
		t.Fatalf("round trip changed the bundle")
	}
	return got
}

// assertMatches checks that reading the bundle's current state gives back the snapshot.
func assertMatches(t *testing.T, b *Bundle, s *snapshot) {
	t.Helper()
	for name, si := range s.Instances {
		inst := b.Instances[name]
		if inst == nil {
			t.Fatalf("%s missing", name)
		}
		if inst.DeletedAt != nil {
			t.Errorf("%s is deleted", name)
		}
		od := inst.LiveOD()
		want := map[odKey]float64{}
		for k, p := range si.OD {
			want[k] = p.Price
		}
		if !reflect.DeepEqual(od, want) {
			t.Errorf("%s on-demand: got %v, want %v", name, od, want)
		}
		rs := inst.LiveRS()
		for k, p := range si.RS {
			if got := rs[k]; got != [2]float64{p.Hourly, p.Upfront} {
				t.Errorf("%s reserved %v: got %v, want %v", name, k, got, p)
			}
		}
		for k := range rs {
			if _, ok := si.RS[k]; !ok && !si.KeepRS[k] {
				t.Errorf("%s reserved %v should not be offered", name, k)
			}
		}
		for region, attrs := range si.Attrs {
			if got := inst.AttrsIn(region); !reflect.DeepEqual(got, attrs) {
				t.Errorf("%s attributes in %q: got %v, want %v", name, region, got, attrs)
			}
		}
	}
	for name, inst := range b.Instances {
		if _, ok := s.Instances[name]; !ok && inst.DeletedAt == nil {
			t.Errorf("%s should be deleted", name)
		}
	}
}

func appendOK(t *testing.T, b *Bundle, s *snapshot) AppendStats {
	t.Helper()
	st, err := b.Append(s)
	if err != nil {
		t.Fatal(err)
	}
	assertMatches(t, b, s)
	roundTrip(t, b)
	return st
}

// --- RunPod ---

type rpGPU map[string]any

func runpodFile(t *testing.T, fetched string, gpus []rpGPU, cpuPrice float64) *snapshot {
	t.Helper()
	gpuJSON, _ := json.Marshal(gpus)
	src := runpodSource{
		Kind:      runpodSourceKind,
		FetchedAt: fetched,
		GpuTypes:  gpuJSON,
		CpuFlavors: json.RawMessage(`[{"id":"cpu3c","groupId":"cpu3","displayName":"Compute-Optimized","minVcpu":2,"maxVcpu":4,
			"vcpuBurstable":false,"ramMultiplier":2,"diskLimitPerVcpu":10}]`),
		DataCenters: json.RawMessage(`[{"id":"US-TX-1","name":"US-TX-1","location":"United States","storageSupport":true,"listed":true,
			"gpuAvailability":[{"gpuTypeId":"NVIDIA H100"},{"gpuTypeId":"NVIDIA A40"}]},
			{"id":"EU-RO-1","name":"EU-RO-1","location":"Europe","storageSupport":false,"listed":true,"gpuAvailability":[{"gpuTypeId":"NVIDIA H100"}]}]`),
		CpuPrices: map[string]json.RawMessage{
			"cpu3c-2-4": json.RawMessage(fmt.Sprintf(`{"securePrice":%g,"slsPrice":0.1,"stockStatus":"High"}`, cpuPrice)),
			"cpu3c-4-8": json.RawMessage(fmt.Sprintf(`{"securePrice":%g,"slsPrice":0.2,"stockStatus":"High"}`, cpuPrice*2)),
		},
	}
	data, _ := json.Marshal(src)
	if !isRunpodSource(data) {
		t.Fatal("fixture is not recognized as a RunPod file")
	}
	s, err := readRunpod(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func h100(secure float64) rpGPU {
	return rpGPU{"id": "NVIDIA H100", "displayName": "H100 SXM", "manufacturer": "Nvidia", "memoryInGb": 80,
		"secureCloud": true, "communityCloud": true, "securePrice": secure, "communityPrice": 2.69,
		"threeMonthPrice": secure - 0.5, "sixMonthPrice": nil, "maxGpuCount": nil, "nodeGroupGpuSizes": []int{8}}
}

func a40() rpGPU {
	// Not sold in Community Cloud, though the API still gives it a community price.
	return rpGPU{"id": "NVIDIA A40", "displayName": "A40", "memoryInGb": 48, "secureCloud": true, "communityCloud": false,
		"securePrice": 0.59, "communityPrice": 0.35}
}

func TestRunpod(t *testing.T) {
	s1 := runpodFile(t, "2026-11-01T00:00:00Z", []rpGPU{h100(3.99), a40(), {"id": "unknown"}}, 0.06)
	if got := slices.Sorted(func(yield func(string) bool) {
		for k := range s1.Instances {
			if !yield(k) {
				return
			}
		}
	}); !reflect.DeepEqual(got, []string{"NVIDIA A40", "NVIDIA H100", "cpu3c-2-4", "cpu3c-4-8"}) {
		t.Fatalf("instances %v", got)
	}
	g := s1.Instances["NVIDIA H100"]
	sec, com := Variant{secureCloud}, Variant{communityCloud}
	if g.OD[odKey{runpodRegion, sec}].Price != 3.99 || g.OD[odKey{runpodRegion, com}].Price != 2.69 {
		t.Errorf("H100 prices %v", g.OD)
	}
	plan := rsKey{runpodRegion, sec, Term{"3mo", "Savings Plan", "All Upfront"}}
	if len(g.RS) != 1 || g.RS[plan].Hourly != 3.49 {
		t.Errorf("H100 savings plans %v", g.RS)
	}
	attrs := g.Attrs[runpodRegion]
	for k, v := range map[string]string{"displayName": "H100 SXM", "memoryInGb": "80", "productFamily": "GPU",
		"priceUnit": "GPU-Hour", "nodeGroupGpuSizes": "8", "dataCenters": "EU-RO-1,US-TX-1", "communityCloud": "true"} {
		if attrs[k] != v {
			t.Errorf("H100 %s = %q, want %q", k, attrs[k], v)
		}
	}
	for _, k := range []string{"id", "securePrice", "sixMonthPrice", "maxGpuCount"} {
		if _, ok := attrs[k]; ok {
			t.Errorf("H100 should not have attribute %s", k)
		}
	}
	if a := s1.Instances["NVIDIA A40"]; len(a.OD) != 1 {
		t.Errorf("A40 should only have a Secure Cloud price: %v", a.OD)
	}
	cpu := s1.Instances["cpu3c-4-8"]
	if cpu.OD[odKey{runpodRegion, sec}].Price != 0.12 || cpu.Attrs[runpodRegion]["memoryInGb"] != "8" || cpu.Attrs[runpodRegion]["vcpu"] != "4" {
		t.Errorf("cpu3c-4-8: %v %v", cpu.OD, cpu.Attrs)
	}
	if dc := s1.Regions["EU-RO-1"]; dc["location"] != "Europe" || dc["storageSupport"] != "false" || dc["locationType"] != "Data Center" {
		t.Errorf("EU-RO-1 region attributes %v", dc)
	}

	b := &Bundle{Instances: map[string]*Instance{}}
	appendOK(t, b, s1)

	// The same prices fetched again add nothing, and the bundle stays as it was.
	before := roundTrip(t, b)
	s2 := runpodFile(t, "2026-11-01T01:00:00Z", []rpGPU{h100(3.99), a40()}, 0.06)
	if _, err := b.Append(s2); !errors.Is(err, errUnchanged) || !errors.Is(err, errNothingNew) {
		t.Fatalf("unchanged fetch: %v", err)
	}
	if !reflect.DeepEqual(b, before) {
		t.Fatal("an unchanged fetch modified the bundle")
	}

	// A price change and a GPU that disappears.
	s3 := runpodFile(t, "2026-11-01T02:00:00Z", []rpGPU{h100(2.99)}, 0.07)
	st := appendOK(t, b, s3)
	if st.Deleted != 1 || st.ODRows != 1+1+2 || st.RSRows != 1 {
		t.Errorf("stats %+v", st)
	}
	if len(b.Snapshots) != 2 {
		t.Errorf("snapshots %v: the unchanged fetch should not be recorded", b.Snapshots)
	}
	if _, err := b.Append(s3); !errors.Is(err, errNotNewer) {
		t.Errorf("same snapshot again: %v", err)
	}
}

func TestRunpodFetch(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		queries = append(queries, req.Query)
		switch {
		case strings.Contains(req.Query, "OtherInstancesRunpodCPU"):
			if !strings.Contains(req.Query, `instanceId: "cpu5m-2-16"`) || !strings.Contains(req.Query, `instanceId: "cpu5m-8-64"`) {
				t.Errorf("CPU query %s", req.Query)
			}
			// Every alias answers for every flavor.
			fmt.Fprint(w, `{"data":{"c0":[{"specifics":{"securePrice":0.1}},{"specifics":{"securePrice":0.1}}],
				"c1":[{"specifics":{"securePrice":0.2}}],"c2":[{"specifics":null}]}}`)
		default:
			fmt.Fprint(w, `{"data":{"gpuTypes":[{"id":"NVIDIA L4","secureCloud":true,"securePrice":0.59}],
				"cpuFlavors":[{"id":"cpu5m","minVcpu":2,"maxVcpu":8,"ramMultiplier":8}],"dataCenters":[]}}`)
		}
	}))
	defer srv.Close()
	src, err := fetchRunpod(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("%d queries", len(queries))
	}
	if len(src.CpuPrices) != 2 || string(src.CpuPrices["cpu5m-4-32"]) != `{"securePrice":0.2}` {
		t.Errorf("CPU prices %v", src.CpuPrices)
	}
	data, _ := json.Marshal(src)
	s, err := readRunpod(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Instances) != 3 {
		t.Errorf("instances %v", s.Instances)
	}

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errors":[{"message":"Cannot query field \"x\""}]}`)
	}))
	defer errSrv.Close()
	if _, err := fetchRunpod(context.Background(), errSrv.Client(), errSrv.URL); err == nil || !strings.Contains(err.Error(), "Cannot query field") {
		t.Errorf("GraphQL error: %v", err)
	}
}

// --- Alibaba ---

type aliFixture struct {
	pub, version string
	prices       map[string]string // key -> pricingInfo JSON
	reserved     string            // reservedInstancePrice JSON, or "" to leave the file out
	defs         string            // instanceTypeDefineItems JSON, or ""
}

func (f aliFixture) write(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var info []string
	for k, v := range f.prices {
		info = append(info, fmt.Sprintf("%q: %s", k, v))
	}
	price := fmt.Sprintf(`{"currency":"USD","version":"x","publicationDate":%q,"pricingInfo":{%s}}`, f.pub, strings.Join(info, ","))
	must(t, os.WriteFile(filepath.Join(dir, aliPriceFile), []byte(price), 0o644))
	if f.reserved != "" {
		must(t, os.WriteFile(filepath.Join(dir, aliReservedFile), []byte("var reservedInstancePrice = "+f.reserved+";\n"), 0o644))
	}
	if f.defs != "" {
		must(t, os.WriteFile(filepath.Join(dir, aliDefsFile), []byte("var instanceTypeDefineItems = "+f.defs), 0o644))
	}
	if f.version != "" {
		must(t, os.WriteFile(filepath.Join(dir, aliSourceFile), []byte(fmt.Sprintf(`{"kind":%q,"version":%q}`, aliSourceKind, f.version)), 0o644))
	}
	return dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func aliPrice(hour, month, year1, year2 string) string {
	return fmt.Sprintf(`{"hours":[{"price":%q,"period":"1","originalPrice":%q}],"months":[{"price":%q,"period":"1"}],
		"years":[{"price":%q,"period":"1","originalPrice":"999"},{"price":%q,"period":"2"}]}`, hour, hour, month, year1, year2)
}

func aliRI(option, hourly, prepay string) string {
	return fmt.Sprintf(`{%q:{"years":[{"reservedHourPrice":%s,"priceUnit":"Year","period":"1","prepayPrice":%q}]}}`, option, hourly, prepay)
}

func TestAlibaba(t *testing.T) {
	v1 := aliFixture{
		pub: "2025-01-01T00:00:00Z", version: "2.0.10",
		prices: map[string]string{
			"cn-hangzhou::ecs.g6.large::vpc::linux::optimized":     aliPrice("0.1", "50", "500", "900"),
			"cn-hangzhou::ecs.g6.large::classic::linux::optimized": aliPrice("0.1", "50", "500", "900"),
			"us-east-1::ecs.g6.large::vpc::windows::optimized":     aliPrice("0.2", "60", "600", "1000"),
			"us-east-1::ecs.g6.large::vpc::linux::none":            aliPrice("9", "9", "9", "9"),
		},
		reserved: `{"us-east-1":{"ecs.g6.large":{"windows":` + aliRI("Partial_Upfront", `"0.05"`, "300") + `,
			"linux":{"No_Upfront":{"years":[{"reservedHourPrice":"0.07","priceUnit":"Year","period":"3","prepayPrice":"0.0"},
				{"reservedHourPrice":null,"priceUnit":"Year","period":"1","prepayPrice":"PriceNotFound"}]}}}}}`,
		defs: `[{"InstanceTypeId":"ecs.g6.large","CpuCoreCount":2,"MemorySize":8.0,"GPUSpec":"","CpuOptions":{"ThreadsPerCore":2},
			"EnhancedNetwork":{},"SupportedBootModes":{"SupportedBootMode":["BIOS","UEFI"]}},{"InstanceTypeId":"ecs.unpriced"}]`,
	}
	s1, err := readAlibaba(v1.write(t))
	if err != nil {
		t.Fatal(err)
	}
	if s1.Version != "2.0.10" || s1.Products != 4 || s1.Skipped != 1 || len(s1.Instances) != 1 {
		t.Fatalf("snapshot: version %s, %d products, %d skipped, %d instances", s1.Version, s1.Products, s1.Skipped, len(s1.Instances))
	}
	g := s1.Instances["ecs.g6.large"]
	linuxVPC, linuxClassic, winVPC := Variant{"Linux", "VPC"}, Variant{"Linux", "Classic"}, Variant{"Windows", "VPC"}
	if g.OD[odKey{"cn-hangzhou", linuxClassic}].Price != 0.1 || g.OD[odKey{"us-east-1", winVPC}].Price != 0.2 || len(g.OD) != 3 {
		t.Errorf("on-demand %v", g.OD)
	}
	sub := func(lease string) Term { return Term{lease, "Subscription", "All Upfront"} }
	if p := g.RS[rsKey{"cn-hangzhou", linuxVPC, sub("1mo")}]; p.Hourly != 0 || p.Upfront != 50 {
		t.Errorf("1 month subscription %v", p)
	}
	if p := g.RS[rsKey{"us-east-1", winVPC, sub("2yr")}]; p.Upfront != 1000 {
		t.Errorf("2 year subscription %v", p)
	}
	ri := rsKey{"us-east-1", winVPC, Term{"1yr", "Reserved Instance", "Partial Upfront"}}
	if p := g.RS[ri]; p.Hourly != 0.05 || p.Upfront != 300 {
		t.Errorf("reserved instance %v", p)
	}
	if _, ok := g.RS[rsKey{"us-east-1", linuxVPC, Term{"1yr", "Reserved Instance", "No Upfront"}}]; ok {
		t.Error("PriceNotFound should not be offered")
	}
	if p := g.RS[rsKey{"us-east-1", linuxVPC, Term{"3yr", "Reserved Instance", "No Upfront"}}]; p.Hourly != 0.07 || p.Upfront != 0 {
		t.Errorf("3 year No Upfront %v", p)
	}
	want := map[string]string{"CpuCoreCount": "2", "MemorySize": "8.0", "CpuOptions.ThreadsPerCore": "2", "SupportedBootModes.SupportedBootMode": "BIOS,UEFI"}
	if !reflect.DeepEqual(g.Attrs[""], want) {
		t.Errorf("attributes %v", g.Attrs[""])
	}
	if r := s1.Regions["us-east-1"]; r["name"] != "US (Virginia)" || r["regionCode"] != "us-east-1" {
		t.Errorf("region %v", r)
	}

	b := &Bundle{Instances: map[string]*Instance{}}
	appendOK(t, b, s1)
	if b.SourceVersion != "2.0.10" {
		t.Errorf("source version %q", b.SourceVersion)
	}

	// Next version: the reserved price was throttled, the classic network is gone, an attribute changed,
	// and nothing changed for a version with the same prices (it is still recorded).
	v2 := v1
	v2.pub, v2.version = "2025-02-01T00:00:00Z", "2.0.11"
	v2.prices = map[string]string{
		"cn-hangzhou::ecs.g6.large::vpc::linux::optimized": aliPrice("0.1", "50", "500", "900"),
		"us-east-1::ecs.g6.large::vpc::windows::optimized": aliPrice("0.2", "60", "600", "1000"),
	}
	v2.reserved = `{"us-east-1":{"ecs.g6.large":{"windows":` + aliRI("Partial_Upfront", "null", "Throttling") + `}}}`
	v2.defs = strings.Replace(v1.defs, `"MemorySize":8.0`, `"MemorySize":16.0`, 1)
	s2, err := readAlibaba(v2.write(t))
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Instances["ecs.g6.large"].KeepRS[ri] || s2.Unknown != 1 {
		t.Fatalf("throttled price should be kept (%d unknown)", s2.Unknown)
	}
	st := appendOK(t, b, s2)
	inst := b.Instances["ecs.g6.large"]
	if p := inst.LiveRS()[ri]; p != [2]float64{0.05, 300} {
		t.Errorf("throttled reserved instance became %v", p)
	}
	// Removed: classic on-demand, its 3 subscriptions, and the 3 year No Upfront reservation.
	if st.ODRows != 1 || st.RSRows != 3+1 || st.AttrChanges != 1 {
		t.Errorf("stats %+v", st)
	}
	if got := inst.AttrsIn("cn-hangzhou")["MemorySize"]; got != "16.0" {
		t.Errorf("MemorySize %q", got)
	}

	// A later version where the price comes back.
	v3 := v2
	v3.pub, v3.version = "2025-03-01T00:00:00Z", "2.0.13"
	v3.reserved = `{"us-east-1":{"ecs.g6.large":{"windows":` + aliRI("Partial_Upfront", `"0.04"`, "250") + `,
		"linux":` + aliRI("No_Upfront", "null", "InvalidVersion") + `}}}`
	s3 := mustReadAli(t, v3.write(t))
	if s3.Unknown != 1 {
		t.Errorf("InvalidVersion should be unknown, got %d unknown", s3.Unknown)
	}
	appendOK(t, b, s3)
	// The same version again is not newer.
	if _, err := b.Append(mustReadAli(t, v3.write(t))); !errors.Is(err, errNotNewer) {
		t.Errorf("same version again: %v", err)
	}
	if b.SourceVersion != "2.0.13" || len(b.Snapshots) != 3 {
		t.Errorf("source version %s, %d snapshots", b.SourceVersion, len(b.Snapshots))
	}
}

func mustReadAli(t *testing.T, dir string) *snapshot {
	t.Helper()
	s, err := readAlibaba(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAlibabaBadPrice(t *testing.T) {
	f := aliFixture{pub: "2025-01-01T00:00:00Z", prices: map[string]string{
		"cn-hangzhou::ecs.g6.large::vpc::linux::optimized": aliPrice("n/a", "1", "1", "1"),
	}}
	if _, err := readAlibaba(f.write(t)); err == nil || !strings.Contains(err.Error(), "bad price") {
		t.Errorf("bad price: %v", err)
	}
	f.prices = map[string]string{"cn-hangzhou::ecs.g6.large::vpc": aliPrice("1", "1", "1", "1")}
	if _, err := readAlibaba(f.write(t)); err == nil {
		t.Error("malformed key should fail")
	}
}

func TestAlibabaVersions(t *testing.T) {
	published := map[string]bool{"2.0.0": true, "2.0.1": true, "2.0.4": true, "2.0.5": true, "2.0.40": true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if r.Method != http.MethodHead || rest != aliPaths[aliPriceFile] || !published[v] {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	got, err := alibabaVersions(ctx, srv.Client(), srv.URL, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	// 2.0.40 is past a gap of more than 10, so it isn't found.
	if !reflect.DeepEqual(got, []string{"2.0.0", "2.0.1", "2.0.4", "2.0.5"}) {
		t.Errorf("versions %v", got)
	}
	got, err = alibabaVersions(ctx, srv.Client(), srv.URL, "2.0.1", 10)
	if err != nil || !reflect.DeepEqual(got, []string{"2.0.4", "2.0.5"}) {
		t.Errorf("versions after 2.0.1: %v %v", got, err)
	}
	if _, err := alibabaVersions(ctx, srv.Client(), srv.URL, "3.1", 10); err == nil {
		t.Error("bad version should fail")
	}
}

func TestAlibabaFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/2.0.7/") {
		case aliPaths[aliPriceFile]:
			fmt.Fprint(w, `{"currency":"USD","publicationDate":"2025-01-01T00:00:00Z","pricingInfo":{"us-east-1::ecs.t5::vpc::linux::optimized":{"hours":[{"price":"0.01","period":"1"}]}}}`)
		case aliPaths[aliDefsFile]:
			fmt.Fprint(w, `var instanceTypeDefineItems = [{"InstanceTypeId":"ecs.t5","CpuCoreCount":1}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "src")
	if err := fetchAlibaba(context.Background(), srv.Client(), srv.URL, "2.0.7", dir); err != nil {
		t.Fatal(err)
	}
	s := mustReadAli(t, dir)
	if s.Version != "2.0.7" || s.Instances["ecs.t5"].Attrs[""]["CpuCoreCount"] != "1" {
		t.Errorf("snapshot %+v", s)
	}
	if err := fetchAlibaba(context.Background(), srv.Client(), srv.URL, "2.0.8", dir); !errors.Is(err, errNotFound) {
		t.Errorf("missing version: %v", err)
	}
}

func TestReadBundleRejectsOtherOffers(t *testing.T) {
	for _, in := range []string{
		`{"compression_table":[],"snapshots":[],"regions":[],"variants":[],"reserved_terms":[],"instances":{}}`,
		`{"offer":"AmazonRDS","compression_table":[],"snapshots":[],"regions":[],"variants":[],"reserved_terms":[],"instances":{}}`,
		`{"offer":"RunpodPods","variant_keys":["x"],"compression_table":[],"snapshots":[],"regions":[],"variants":[],"reserved_terms":[],"instances":{}}`,
	} {
		if _, err := ReadBundle(strings.NewReader(in)); err == nil {
			t.Errorf("should fail: %s", in)
		}
	}
}
