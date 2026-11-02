package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Google Cloud publishes Compute Engine prices through the Cloud Billing Catalog API, which only has current
// prices and needs credentials (see gcpauth.go), so the update script reads it on a schedule: `fetch gcp`
// saves one read as a source file, which compress and append turn into a snapshot dated by when it was
// fetched.
//
// Most machine types have no price of their own. They are billed per vCPU and per GiB of memory at their
// family's rates, plus their GPUs, so the reader works out each machine type's price from its shape, which
// comes from the Compute Engine API's machine type list (that list also says which zones have it).

const (
	gcpBillingURL = "https://cloudbilling.googleapis.com/v1/services/6F81-5844-456A/skus" // Compute Engine
	gcpComputeURL = "https://compute.googleapis.com/compute/v1/projects"
)

// gcpSourceKind marks a fetched GCP file; compress and append look for it to recognize one.
const gcpSourceKind = "gcp-compute-engine"

const (
	gcpStandard = "Standard"
	gcpSpot     = "Spot"
)

// gcpSource is a fetched GCP file: Compute Engine's SKUs, its machine types in every zone, and when they
// were read.
type gcpSource struct {
	Kind         string           `json:"kind"`
	FetchedAt    string           `json:"fetchedAt"`
	Skus         []gcpSku         `json:"skus"`
	MachineTypes []map[string]any `json:"machineTypes"` // with "zone" shortened to the zone's name
}

type gcpSku struct {
	Description string `json:"description"`
	Category    struct {
		ResourceFamily string `json:"resourceFamily"`
		ResourceGroup  string `json:"resourceGroup"`
		UsageType      string `json:"usageType"`
	} `json:"category"`
	ServiceRegions []string `json:"serviceRegions"`
	PricingInfo    []struct {
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
	} `json:"pricingInfo"`
}

// gcpMachineFields are the machine type fields that aren't attributes: ids, links, and the zone, which is
// recorded as the zones list instead.
var gcpMachineFields = map[string]bool{"kind": true, "id": true, "selfLink": true, "creationTimestamp": true, "name": true, "zone": true}

// fetchGCP reads Compute Engine's SKUs and machine types.
func fetchGCP(ctx context.Context, client *http.Client, billingURL, computeURL string, creds gcpCreds) (*gcpSource, error) {
	fetched := time.Now().UTC()
	src := &gcpSource{Kind: gcpSourceKind, FetchedAt: fetched.Format(time.RFC3339)}
	for token := ""; ; {
		q := url.Values{"pageSize": {"5000"}, "currencyCode": {"USD"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Skus          []gcpSku `json:"skus"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := getJSONAuth(ctx, client, billingURL+"?"+q.Encode(), creds.Token, &page); err != nil {
			return nil, fmt.Errorf("Cloud Billing Catalog API: %w", err)
		}
		for _, s := range page.Skus {
			if s.Category.ResourceFamily == "Compute" {
				src.Skus = append(src.Skus, s)
			}
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	listURL := fmt.Sprintf("%s/%s/aggregated/machineTypes", computeURL, url.PathEscape(creds.Project))
	for token := ""; ; {
		q := url.Values{"maxResults": {"500"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Items map[string]struct {
				MachineTypes []map[string]any `json:"machineTypes"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := getJSONAuth(ctx, client, listURL+"?"+q.Encode(), creds.Token, &page); err != nil {
			return nil, fmt.Errorf("Compute Engine machine types: %w", err)
		}
		for _, scope := range slices.Sorted(maps.Keys(page.Items)) {
			for _, mt := range page.Items[scope].MachineTypes {
				if z, _ := mt["zone"].(string); z != "" {
					mt["zone"] = z[strings.LastIndexByte(z, '/')+1:]
				}
				for k := range mt {
					if k != "zone" && k != "name" && gcpMachineFields[k] {
						delete(mt, k)
					}
				}
				src.MachineTypes = append(src.MachineTypes, mt)
			}
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if len(src.Skus) == 0 || len(src.MachineTypes) == 0 {
		return nil, fmt.Errorf("GCP returned %d Compute Engine SKUs and %d machine types", len(src.Skus), len(src.MachineTypes))
	}
	return src, nil
}

// gcpRate is one component of a machine type's price, in one region, for one way of paying.
type gcpRate struct {
	stem   string // the SKU family, normalized by gcpStem ("n2", "computeoptimized", "nvidial4gpu")
	kind   string // core, ram (per GiB), gpu, or machine (f1-micro and g1-small have their own SKUs)
	model  string // Standard, Spot, 1yr or 3yr (commitments)
	region string
}

// gcpSkip marks SKUs that aren't the price of an ordinary VM: sole tenant nodes, custom machine types,
// reservations, Dynamic Workload Scheduler, premiums and so on.
var gcpSkip = []string{"Sole Tenancy", "Custom", "Premium", "Reserved", "DWS", "Extended", "Calendar Mode", "Flex Start",
	"vGPU", "Workstation", "Overcommit", "Upgrade", "Local SSD"}

// gcpStem normalizes a SKU family name so the same family matches across descriptions: "C4A Arm Instance",
// "C4A Arm" and "C4A" are all "c4a".
func gcpStem(s string) string {
	s = strings.ToLower(s)
	for _, w := range []string{"instance", " amd", " arm", "predefined", " "} {
		s = strings.ReplaceAll(s, w, "")
	}
	return s
}

// gcpParseSku reads which price a SKU is, and its price per hour (per GiB-hour for memory).
func gcpParseSku(s gcpSku) (rates []gcpRate, price float64, ok bool) {
	d := s.Description
	for _, w := range gcpSkip {
		if strings.Contains(d, w) {
			return nil, 0, false
		}
	}
	model := ""
	switch s.Category.UsageType {
	case "OnDemand":
		model = gcpStandard
	case "Preemptible":
		model = gcpSpot
	case "Commit1Yr":
		model = "1yr"
	case "Commit3Yr":
		model = "3yr"
	default:
		return nil, 0, false
	}
	if strings.Contains(d, "Preemptible") {
		if model != gcpStandard && model != gcpSpot {
			return nil, 0, false
		}
		model = gcpSpot
	}
	commitment := strings.HasPrefix(d, "Commitment")
	d = strings.TrimPrefix(d, "Commitment v1: ")
	d = strings.TrimPrefix(d, "Commitment: ")
	d = strings.TrimPrefix(d, "Spot Preemptible ")
	d = strings.Replace(d, " attached to Spot Preemptible VMs", "", 1)
	if i := strings.Index(d, " running in "); i >= 0 {
		d = d[:i]
	} else if i := strings.LastIndex(d, " in "); i >= 0 {
		d = d[:i]
	}

	kind := ""
	switch g := s.Category.ResourceGroup; g {
	case "CPU", "RAM", "N1Standard":
		// The first commitments have no family: "Commitment v1: Cpu".
		d = " " + d
		for suf, k := range map[string]string{" Core": "core", " Cpu": "core", " Ram": "ram"} {
			if strings.HasSuffix(d, suf) {
				d, kind = strings.TrimSuffix(d, suf), k
			}
		}
	case "GPU":
		kind = "gpu"
	case "F1Micro":
		kind, d = "machine", "f1-micro"
	case "G1Small":
		kind, d = "machine", "g1-small"
	}
	stem := gcpStem(d)
	if commitment && stem == "" && kind != "gpu" {
		stem = "n1" // the first commitments, "Commitment v1: Cpu", are N1's
	}
	if kind == "" || stem == "" || len(s.PricingInfo) == 0 {
		return nil, 0, false
	}
	expr := s.PricingInfo[0].PricingExpression
	if len(expr.TieredRates) == 0 {
		return nil, 0, false
	}
	// Compute prices have one tier; a free first tier would come before the real price.
	tier := expr.TieredRates[0]
	if len(expr.TieredRates) > 1 && tier.UnitPrice.Units == "0" && tier.UnitPrice.Nanos == 0 {
		tier = expr.TieredRates[1]
	}
	if tier.UnitPrice.CurrencyCode != "" && tier.UnitPrice.CurrencyCode != "USD" {
		return nil, 0, false
	}
	units, err := strconv.ParseFloat(tier.UnitPrice.Units, 64)
	if err != nil {
		return nil, 0, false
	}
	price = units + float64(tier.UnitPrice.Nanos)/1e9
	switch {
	case kind == "ram" && expr.UsageUnit == "GiBy.h":
	case kind == "ram" && expr.UsageUnit == "GBy.h":
		price *= (1 << 30) / 1e9 // a few families are priced per GB; a GiB is 1.073741824 GB
	case kind != "ram" && expr.UsageUnit == "h":
	default:
		return nil, 0, false
	}
	for _, r := range s.ServiceRegions {
		rates = append(rates, gcpRate{stem: stem, kind: kind, model: model, region: r})
	}
	return rates, price, len(rates) > 0
}

// gcpFamilyStems are the SKU families of machine types whose name doesn't start with their family's SKU
// name, longest prefix first. Machine types not listed use the part of their name before the first "-".
var gcpFamilyStems = []struct{ prefix, stem string }{
	{"z4d-highmem-", ""}, // by local SSD size, below
	{"h4d-megamem-192", "h4d-megamem-192"},
	{"m4n-ultramem-224", "m4nultramem224"},
	{"m4-ultramem-224", "m4ultramem224"},
	{"a3-megagpu-", "a3plus"},
	{"a3-ultragpu-", "a3ultra"},
	{"a3-highgpu-", "a3"},
	{"a3-edgegpu-", "a3"},
	{"c2-", "computeoptimized"},
	{"m1-", "memory-optimized"},
	{"m2-", "memory-optimized"},
	{"m3-", "m3memory-optimized"},
	// A4 is billed by GPU slice, each with its share of the vCPUs and memory.
	{"a4-", "-"},
}

// gcpGPUStems are the SKU families of the GPUs machine types come with.
var gcpGPUStems = map[string]string{
	"nvidia-tesla-a100":     "nvidiateslaa100gpu",
	"nvidia-a100-80gb":      "nvidiateslaa10080gbgpu",
	"nvidia-h100-80gb":      "nvidiah10080gbgpu",
	"nvidia-h100-mega-80gb": "nvidiah10080gbmegagpu",
	"nvidia-h200-141gb":     "h200141gbgpu",
	"nvidia-l4":             "nvidial4gpu",
	"nvidia-rtx-pro-6000":   "rtx600096gb",
	"nvidia-b200":           "a4nvidiab200(1gpuslice)",
}

// gcpSharedCPUs are the shares of a vCPU shared-core E2 machine types are billed for.
var gcpSharedCPUs = map[string]float64{"e2-micro": 0.25, "e2-small": 0.5, "e2-medium": 1}

// gcpMachineStem is the SKU family of a machine type's vCPUs and memory, "-" if they are part of its GPUs'
// price.
func gcpMachineStem(name string) string {
	for _, f := range gcpFamilyStems {
		if !strings.HasPrefix(name, f.prefix) {
			continue
		}
		if f.prefix == "z4d-highmem-" {
			switch {
			case strings.Contains(name, "-highlssd"):
				return "z4d-highmem-highlssd"
			case strings.Contains(name, "-standardlssd"):
				return "z4d-highmem-standardlssd"
			}
			return ""
		}
		return f.stem
	}
	family, _, _ := strings.Cut(name, "-")
	return family
}

// gcpMachine is one machine type, as listed in each zone that has it.
type gcpMachine struct {
	Name         string
	Zones        map[string][]string // region -> zones
	Attrs        map[string]string
	CPUs         float64
	MemoryGiB    float64
	Accelerators map[string]float64 // accelerator type -> count
}

// readGCP turns a fetched GCP file into a snapshot.
func readGCP(r io.Reader) (*snapshot, error) {
	var src gcpSource
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("decoding GCP file: %w", err)
	}
	if src.Kind != gcpSourceKind {
		return nil, fmt.Errorf("not a GCP file (kind %q)", src.Kind)
	}
	ts, err := parseTS(src.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("fetchedAt: %w", err)
	}
	s := &snapshot{Offer: gcpOffer, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}}

	rates := map[gcpRate]float64{}
	for _, sku := range src.Skus {
		keys, price, ok := gcpParseSku(sku)
		if !ok {
			continue
		}
		for _, k := range keys {
			// A few regions list two SKUs for one family ("Memory-optimized ... in Virginia" and "... in
			// Northern Virginia"); the lower price is kept, whichever order they come in.
			if prev, dup := rates[k]; dup {
				if prev != price {
					s.Collisions++
				}
				price = min(prev, price)
			}
			rates[k] = price
		}
	}

	machines := map[string]*gcpMachine{}
	for _, mt := range src.MachineTypes {
		name, _ := mt["name"].(string)
		zone, _ := mt["zone"].(string)
		i := strings.LastIndexByte(zone, '-')
		if name == "" || i < 0 {
			continue
		}
		region := zone[:i]
		m := machines[name]
		if m == nil {
			m = &gcpMachine{Name: name, Zones: map[string][]string{}, Attrs: map[string]string{"productFamily": "Compute Engine"},
				Accelerators: map[string]float64{}}
			for k, v := range mt {
				if !gcpMachineFields[k] && k != "accelerators" && k != "deprecated" {
					flatten(k, v, m.Attrs)
				}
			}
			if dep, ok := mt["deprecated"].(map[string]any); ok {
				flatten("deprecated", dep["state"], m.Attrs)
			}
			accs, _ := mt["accelerators"].([]any)
			var names []string
			for _, a := range accs {
				am, _ := a.(map[string]any)
				t, _ := am["guestAcceleratorType"].(string)
				n, _ := strconv.ParseFloat(fmt.Sprint(am["guestAcceleratorCount"]), 64)
				if t != "" && n > 0 {
					m.Accelerators[t] += n
					names = append(names, t)
					m.Attrs["acceleratorCount"] = strconv.FormatFloat(m.Accelerators[t], 'f', -1, 64)
				}
			}
			if len(names) > 0 {
				m.Attrs["acceleratorType"] = strings.Join(names, ",")
			}
			m.CPUs, _ = strconv.ParseFloat(fmt.Sprint(mt["guestCpus"]), 64)
			mem, _ := strconv.ParseFloat(fmt.Sprint(mt["memoryMb"]), 64)
			m.MemoryGiB = mem / 1024
			if share, ok := gcpSharedCPUs[name]; ok {
				m.CPUs = share
			}
			machines[name] = m
		}
		if !slices.Contains(m.Zones[region], zone) {
			m.Zones[region] = append(m.Zones[region], zone)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(machines)) {
		m := machines[name]
		s.Products++
		si := newSnapInstance()
		for region, zones := range m.Zones {
			for _, model := range []string{gcpStandard, gcpSpot, "1yr", "3yr"} {
				p, ok := m.price(rates, region, model)
				if !ok {
					continue
				}
				switch model {
				case gcpStandard, gcpSpot:
					s.setOD(si, odKey{Region: region, Variant: Variant{model}}, p)
				default:
					s.setRS(si, rsKey{Region: region, Variant: Variant{gcpStandard}, Term: Term{model, "Committed Use", "No Upfront"}}, p, 0)
				}
			}
			// Prices are only recorded where every part of the machine type's price is known.
			if !hasRegion(si, region) {
				continue
			}
			slices.Sort(zones)
			attrs := maps.Clone(m.Attrs)
			attrs["zones"] = strings.Join(zones, ",")
			si.Attrs[region] = attrs
			if s.Regions[region] == nil {
				s.Regions[region] = map[string]string{"regionCode": region, "locationType": "Google Cloud Region"}
			}
		}
		if len(si.OD) == 0 && len(si.RS) == 0 {
			s.Unpriced++
			continue
		}
		s.Instances[name] = si
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("GCP file has no machine type prices")
	}
	return s, nil
}

func hasRegion(si *snapInstance, region string) bool {
	for k := range si.OD {
		if k.Region == region {
			return true
		}
	}
	for k := range si.RS {
		if k.Region == region {
			return true
		}
	}
	return false
}

// price is the machine type's hourly price in a region: its vCPUs and memory at its family's rates, plus
// its GPUs. ok is false if any part has no price there.
func (m *gcpMachine) price(rates map[gcpRate]float64, region, model string) (float64, bool) {
	if m.Name == "f1-micro" || m.Name == "g1-small" {
		if model != gcpStandard && model != gcpSpot {
			return 0, false
		}
		p, ok := rates[gcpRate{stem: m.Name, kind: "machine", model: model, region: region}]
		return p, ok
	}
	if _, shared := gcpSharedCPUs[m.Name]; shared && model != gcpStandard && model != gcpSpot {
		return 0, false // committed use discounts don't cover shared-core machine types
	}
	total := 0.0
	stem := gcpMachineStem(m.Name)
	switch stem {
	case "":
		return 0, false
	case "-":
		if len(m.Accelerators) == 0 {
			return 0, false
		}
	default:
		core, ok1 := rates[gcpRate{stem: stem, kind: "core", model: model, region: region}]
		ram, ok2 := rates[gcpRate{stem: stem, kind: "ram", model: model, region: region}]
		if !ok1 || !ok2 {
			return 0, false
		}
		total += m.CPUs*core + m.MemoryGiB*ram
	}
	for t, n := range m.Accelerators {
		gs, known := gcpGPUStems[t]
		p, ok := rates[gcpRate{stem: gs, kind: "gpu", model: model, region: region}]
		if !known || !ok {
			return 0, false
		}
		total += n * p
	}
	// Round away float noise from adding the parts, at a millionth of a cent.
	return roundPrice(total), true
}

func roundPrice(p float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(p, 'f', 8, 64), 64)
	return r
}
