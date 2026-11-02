package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RunPod publishes no price list. Its GraphQL API answers price queries without an API key, so the update
// script reads it on a schedule: `fetch runpod` saves one read as a source file, which compress and append
// turn into a snapshot dated by when it was fetched.
//
// GPU types are priced per GPU per hour, the same in every data center. Secure Cloud also sells savings
// plans, which are paid up front for a term and quoted as an effective hourly rate. CPU pods are priced per
// pod per hour, by flavor and size.

const runpodGraphQL = "https://api.runpod.io/graphql"

// runpodSourceKind marks a fetched RunPod file; compress and append look for it to recognize one.
const runpodSourceKind = "runpod-graphql"

const runpodRegion = "global"

const (
	secureCloud    = "Secure Cloud"
	communityCloud = "Community Cloud"
)

// runpodPlans maps the GPU fields that hold savings plan rates to their lease length.
var runpodPlans = []struct{ field, lease string }{
	{"oneWeekPrice", "1wk"},
	{"oneMonthPrice", "1mo"},
	{"threeMonthPrice", "3mo"},
	{"sixMonthPrice", "6mo"},
}

// runpodPriceFields are the GPU fields that are prices or price lookups rather than attributes.
var runpodPriceFields = map[string]bool{
	"id": true, "securePrice": true, "communityPrice": true, "secureSpotPrice": true, "communitySpotPrice": true,
	"oneWeekPrice": true, "oneMonthPrice": true, "threeMonthPrice": true, "sixMonthPrice": true, "lowestPrice": true,
}

const runpodMainQuery = `query OtherInstancesRunpod {
  gpuTypes {
    id displayName manufacturer memoryInGb cudaCores secureCloud communityCloud
    securePrice communityPrice secureSpotPrice communitySpotPrice
    oneWeekPrice oneMonthPrice threeMonthPrice sixMonthPrice
    maxGpuCount maxGpuCountSecureCloud maxGpuCountCommunityCloud minPodGpuCount nodeGroupGpuSizes throughput
  }
  cpuFlavors { id groupId displayName minVcpu maxVcpu vcpuBurstable ramMultiplier diskLimitPerVcpu }
  dataCenters { id name location storageSupport listed gpuAvailability { gpuTypeId } }
}`

// runpodSource is a fetched RunPod file: the API's answers, as it gave them, and when they were read.
type runpodSource struct {
	Kind        string                     `json:"kind"`
	FetchedAt   string                     `json:"fetchedAt"`
	GpuTypes    json.RawMessage            `json:"gpuTypes"`
	CpuFlavors  json.RawMessage            `json:"cpuFlavors"`
	DataCenters json.RawMessage            `json:"dataCenters"`
	CpuPrices   map[string]json.RawMessage `json:"cpuPrices"` // CPU pod id -> its specifics
}

type runpodCPUFlavor struct {
	ID            string      `json:"id"`
	MinVcpu       int         `json:"minVcpu"`
	MaxVcpu       int         `json:"maxVcpu"`
	RamMultiplier json.Number `json:"ramMultiplier"`
}

// cpuPodSizes lists a flavor's pod ids, such as cpu3c-2-4: the flavor, the vCPUs (powers of two from its
// minimum to its maximum) and the memory in GB (vCPUs times the flavor's RAM multiplier).
func (f runpodCPUFlavor) cpuPodSizes() ([]cpuPodSize, error) {
	mult, err := strconv.ParseFloat(f.RamMultiplier.String(), 64)
	if err != nil || mult <= 0 {
		return nil, fmt.Errorf("CPU flavor %s has RAM multiplier %q", f.ID, f.RamMultiplier)
	}
	if f.MinVcpu < 1 || f.MaxVcpu < f.MinVcpu {
		return nil, fmt.Errorf("CPU flavor %s has %d to %d vCPUs", f.ID, f.MinVcpu, f.MaxVcpu)
	}
	var out []cpuPodSize
	for v := f.MinVcpu; v <= f.MaxVcpu; v *= 2 {
		mem := strconv.FormatFloat(float64(v)*mult, 'f', -1, 64)
		out = append(out, cpuPodSize{ID: fmt.Sprintf("%s-%d-%s", f.ID, v, mem), Vcpu: v, MemoryGB: mem})
	}
	return out, nil
}

type cpuPodSize struct {
	ID       string
	Vcpu     int
	MemoryGB string
}

// fetchRunpod reads RunPod's GPU types, CPU flavors, data centers and CPU pod prices.
func fetchRunpod(ctx context.Context, client *http.Client, endpoint string) (*runpodSource, error) {
	var main struct {
		GpuTypes    json.RawMessage `json:"gpuTypes"`
		CpuFlavors  json.RawMessage `json:"cpuFlavors"`
		DataCenters json.RawMessage `json:"dataCenters"`
	}
	fetched := time.Now().UTC()
	if err := runpodQuery(ctx, client, endpoint, runpodMainQuery, &main); err != nil {
		return nil, err
	}
	var flavors []runpodCPUFlavor
	if err := json.Unmarshal(main.CpuFlavors, &flavors); err != nil {
		return nil, fmt.Errorf("RunPod cpuFlavors: %w", err)
	}
	// A CPU pod's price comes from cpuFlavors' specifics, which prices the pod id it is given. Every alias
	// returns every flavor with the same answer, so only the first entry is kept.
	var q strings.Builder
	q.WriteString("query OtherInstancesRunpodCPU {")
	var ids []string
	for _, f := range flavors {
		sizes, err := f.cpuPodSizes()
		if err != nil {
			return nil, err
		}
		for _, s := range sizes {
			fmt.Fprintf(&q, " c%d: cpuFlavors { specifics(input: {instanceId: %q}) { securePrice slsPrice stockStatus } }", len(ids), s.ID)
			ids = append(ids, s.ID)
		}
	}
	q.WriteString(" }")
	prices := map[string]json.RawMessage{}
	if len(ids) > 0 {
		var answers map[string][]struct {
			Specifics json.RawMessage `json:"specifics"`
		}
		if err := runpodQuery(ctx, client, endpoint, q.String(), &answers); err != nil {
			return nil, err
		}
		for i, id := range ids {
			a := answers[fmt.Sprintf("c%d", i)]
			if len(a) > 0 && len(a[0].Specifics) > 0 && string(a[0].Specifics) != "null" {
				prices[id] = a[0].Specifics
			}
		}
	}
	return &runpodSource{
		Kind:        runpodSourceKind,
		FetchedAt:   fetched.Format(time.RFC3339),
		GpuTypes:    main.GpuTypes,
		CpuFlavors:  main.CpuFlavors,
		DataCenters: main.DataCenters,
		CpuPrices:   prices,
	}, nil
}

func runpodQuery(ctx context.Context, client *http.Client, endpoint, query string, dst any) error {
	body, _ := json.Marshal(map[string]string{"query": query})
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "other-instances")
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			last = fmt.Errorf("RunPod API: %s", resp.Status)
			continue
		}
		var out struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return fmt.Errorf("RunPod API (%s): %w", resp.Status, err)
		}
		if len(out.Errors) > 0 {
			msgs := make([]string, len(out.Errors))
			for i, e := range out.Errors {
				msgs[i] = e.Message
			}
			return fmt.Errorf("RunPod API: %s", strings.Join(msgs, "; "))
		}
		if resp.StatusCode != http.StatusOK || len(out.Data) == 0 || string(out.Data) == "null" {
			return fmt.Errorf("RunPod API: %s with no data", resp.Status)
		}
		return json.Unmarshal(out.Data, dst)
	}
	return last
}

// isRunpodSource reports whether the file starting with head is a fetched RunPod file.
func isRunpodSource(head []byte) bool {
	return sourceKind(head) == runpodSourceKind
}

// readRunpod turns a fetched RunPod file into a snapshot.
func readRunpod(r io.Reader) (*snapshot, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var src runpodSource
	if err := json.Unmarshal(data, &src); err != nil {
		return nil, fmt.Errorf("decoding RunPod file: %w", err)
	}
	if src.Kind != runpodSourceKind {
		return nil, fmt.Errorf("not a RunPod file (kind %q)", src.Kind)
	}
	ts, err := parseTS(src.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("fetchedAt: %w", err)
	}
	s := &snapshot{Offer: runpodOffer, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{
		runpodRegion: {"regionCode": runpodRegion, "locationType": "Global"},
	}}

	var dcs []map[string]any
	if err := decodeNumbers(src.DataCenters, &dcs); err != nil {
		return nil, fmt.Errorf("dataCenters: %w", err)
	}
	gpuDCs := map[string][]string{}
	for _, dc := range dcs {
		id, _ := dc["id"].(string)
		if id == "" {
			continue
		}
		attrs := map[string]string{"regionCode": id, "locationType": "Data Center"}
		for k, v := range dc {
			if k != "id" && k != "gpuAvailability" {
				flatten(k, v, attrs)
			}
		}
		s.Regions[id] = attrs
		avail, _ := dc["gpuAvailability"].([]any)
		for _, a := range avail {
			m, _ := a.(map[string]any)
			if g, _ := m["gpuTypeId"].(string); g != "" && !slices.Contains(gpuDCs[g], id) {
				gpuDCs[g] = append(gpuDCs[g], id)
			}
		}
	}

	var gpus []map[string]any
	if err := decodeNumbers(src.GpuTypes, &gpus); err != nil {
		return nil, fmt.Errorf("gpuTypes: %w", err)
	}
	for _, g := range gpus {
		id, _ := g["id"].(string)
		if id == "" {
			continue
		}
		s.Products++
		si := newSnapInstance()
		secure, community := g["secureCloud"] == true, g["communityCloud"] == true
		// The API keeps a price for clouds that don't sell the GPU, so only the clouds that do are recorded.
		if p := price(g["securePrice"]); secure && p > 0 {
			s.setOD(si, odKey{Region: runpodRegion, Variant: Variant{secureCloud}}, p)
		}
		if p := price(g["communityPrice"]); community && p > 0 {
			s.setOD(si, odKey{Region: runpodRegion, Variant: Variant{communityCloud}}, p)
		}
		if secure {
			for _, plan := range runpodPlans {
				if p := price(g[plan.field]); p > 0 {
					k := rsKey{Region: runpodRegion, Variant: Variant{secureCloud}, Term: Term{plan.lease, "Savings Plan", "All Upfront"}}
					s.setRS(si, k, p, 0)
				}
			}
		}
		if len(si.OD) == 0 && len(si.RS) == 0 {
			continue
		}
		attrs := map[string]string{"productFamily": "GPU", "priceUnit": "GPU-Hour"}
		for k, v := range g {
			if !runpodPriceFields[k] {
				flatten(k, v, attrs)
			}
		}
		if dc := gpuDCs[id]; len(dc) > 0 {
			slices.Sort(dc)
			attrs["dataCenters"] = strings.Join(dc, ",")
		}
		si.Attrs[runpodRegion] = attrs
		s.Instances[id] = si
	}

	var flavors []map[string]any
	if err := decodeNumbers(src.CpuFlavors, &flavors); err != nil {
		return nil, fmt.Errorf("cpuFlavors: %w", err)
	}
	for _, raw := range flavors {
		var f runpodCPUFlavor
		if err := json.Unmarshal([]byte(compactJSON(raw)), &f); err != nil {
			return nil, fmt.Errorf("cpuFlavors: %w", err)
		}
		sizes, err := f.cpuPodSizes()
		if err != nil {
			return nil, err
		}
		for _, size := range sizes {
			s.Products++
			var spec map[string]any
			if b, ok := src.CpuPrices[size.ID]; ok {
				if err := decodeNumbers(b, &spec); err != nil {
					return nil, fmt.Errorf("CPU pod %s: %w", size.ID, err)
				}
			}
			p := price(spec["securePrice"])
			if p <= 0 {
				continue
			}
			if _, dup := s.Instances[size.ID]; dup {
				return nil, fmt.Errorf("CPU pod %s has the same name as a GPU type", size.ID)
			}
			si := newSnapInstance()
			s.setOD(si, odKey{Region: runpodRegion, Variant: Variant{secureCloud}}, p)
			attrs := map[string]string{
				"productFamily": "CPU", "priceUnit": "Hour", "flavor": f.ID,
				"vcpu": strconv.Itoa(size.Vcpu), "memoryInGb": size.MemoryGB,
			}
			for k, v := range raw {
				switch k {
				case "id", "minVcpu", "maxVcpu":
				default:
					flatten(k, v, attrs)
				}
			}
			si.Attrs[runpodRegion] = attrs
			s.Instances[size.ID] = si
		}
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("RunPod file has no prices")
	}
	return s, nil
}

// price reads a JSON number as a price; anything else (null, missing) is 0.
func price(v any) float64 {
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return 0
	}
	return f
}
