package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Oracle publishes a price list of parts (https://apexapps.oracle.com/.../cetools/api/v1/products), each billed
// per OCPU, GB, GPU and so on. Which parts make up each compute shape comes from the data behind Oracle's cost
// estimator (productpresets.json). Neither needs a key or has any history, so `fetch oracle` saves one read of
// the two, with when it was read.
//
// Oracle charges the same in every commercial region, so every row is in "global".

const (
	oraclePartsURL   = "https://apexapps.oracle.com/pls/apex/cetools/api/v1/products/?currencyCode=USD"
	oraclePresetsURL = "https://www.oracle.com/a/ocom/docs/cloudestimator2/data/productpresets.json"
)

const oracleSourceKind = "oracle-compute-prices"

const oracleRegion = "global"

// oracleWindowsPart is the display name of the part Windows images are billed by, per OCPU.
const oracleWindowsPart = "Compute - Windows OS"

// oracleMemoryPerOCPU are the GB per OCPU a flexible shape is priced at. Any other amount can be worked out from
// the shape's ocpuPrice and memoryGbPrice attributes.
var oracleMemoryPerOCPU = []int{4, 8, 16}

type oraclePart struct {
	PartNumber      string `json:"partNumber"`
	DisplayName     string `json:"displayName"`
	MetricName      string `json:"metricName"`
	ServiceCategory string `json:"serviceCategory"`
	// Prices are in the one currency fetch asked for. A part with a free tier has one price per usage range.
	CurrencyCodeLocalizations []struct {
		CurrencyCode string `json:"currencyCode"`
		Prices       []struct {
			Model string  `json:"model"`
			Value float64 `json:"value"`
		} `json:"prices"`
	} `json:"currencyCodeLocalizations"`
}

// usd is what the part costs beyond any free tier.
func (p oraclePart) usd() (float64, bool) {
	price, found := 0.0, false
	for _, c := range p.CurrencyCodeLocalizations {
		if c.CurrencyCode != "USD" {
			continue
		}
		for _, v := range c.Prices {
			if v.Model == "PAY_AS_YOU_GO" {
				price, found = max(price, v.Value), true
			}
		}
	}
	return price, found
}

type oracleLabel struct {
	Value string `json:"value"`
}

type oracleShapePart struct {
	Type       oracleLabel `json:"type"` // ocpu, memory or disk
	PartNumber string      `json:"partNumber"`
	Qty        float64     `json:"qty"`
	Max        *float64    `json:"max"`
}

type oracleShape struct {
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	Description      string            `json:"description"`
	ShapeType        oracleLabel       `json:"shapeType"`
	SubType          oracleLabel       `json:"subType"`
	ProcessorType    oracleLabel       `json:"processorType"`
	Products         []oracleShapePart `json:"products"`
	BundleMemoryQty  float64           `json:"bundleMemoryQty"`
	GpuQty           *float64          `json:"gpuQty"`
	GpuMemoryQty     *float64          `json:"gpuMemoryQty"`
	LocalDisk        string            `json:"localDisk"`
	WindowsSupported bool              `json:"windowsSupported"`
	AllowPreemptible bool              `json:"allowPreemptible"`
	AllowBurstable   bool              `json:"allowBurstable"`
}

type oracleSource struct {
	Kind      string        `json:"kind"`
	FetchedAt string        `json:"fetchedAt"`
	Parts     []oraclePart  `json:"parts"`
	Shapes    []oracleShape `json:"shapes"`
}

// fetchOracle reads the price list and every VM and bare metal shape. Oracle also lists shapes for Container
// Instances, dedicated VM hosts, Kubernetes pods and managed databases, which aren't instances of Compute.
func fetchOracle(ctx context.Context, client *http.Client, partsURL, presetsURL string) (*oracleSource, error) {
	fetched := time.Now().UTC()
	var parts struct {
		Items []oraclePart `json:"items"`
	}
	if err := getJSON(ctx, client, partsURL, &parts); err != nil {
		return nil, fmt.Errorf("Oracle price list: %w", err)
	}
	if len(parts.Items) == 0 {
		return nil, fmt.Errorf("Oracle price list is empty")
	}
	var presets struct {
		Items []struct {
			PresetShapes []oracleShape `json:"presetShapes"`
		} `json:"items"`
	}
	if err := getJSON(ctx, client, presetsURL, &presets); err != nil {
		return nil, fmt.Errorf("Oracle cost estimator: %w", err)
	}
	// A shape appears in every product that can use it, sometimes with only some of its parts.
	shapes := map[string]oracleShape{}
	for _, preset := range presets.Items {
		for _, shape := range preset.PresetShapes {
			if !strings.HasPrefix(shape.Name, "VM.") && !strings.HasPrefix(shape.Name, "BM.") {
				continue
			}
			if prev, ok := shapes[shape.Name]; !ok || len(shape.Products) > len(prev.Products) {
				shapes[shape.Name] = shape
			}
		}
	}
	if len(shapes) == 0 {
		return nil, fmt.Errorf("Oracle cost estimator lists no compute shapes")
	}
	src := &oracleSource{Kind: oracleSourceKind, FetchedAt: fetched.Format(time.RFC3339), Parts: parts.Items}
	for _, name := range slices.Sorted(maps.Keys(shapes)) {
		src.Shapes = append(src.Shapes, shapes[name])
	}
	return src, nil
}

// oracleSize is one priced size of a shape. A fixed shape has one size, and a flexible shape several.
type oracleSize struct {
	name             string
	ocpus, memory    float64
	disk             float64 // TB
	flexible         bool
	maxOCPUs, maxMem float64
}

// oracleSizes lists the sizes of a shape. Flexible shapes are priced at powers of two OCPUs and their maximum,
// each at oracleMemoryPerOCPU. Flexible Dense I/O shapes come in steps of 8 OCPUs with memory and NVMe in
// proportion, so those are priced from 8 OCPUs up.
func oracleSizes(shape oracleShape) []oracleSize {
	var ocpu, mem, disk *oracleShapePart
	for i, part := range shape.Products {
		switch part.Type.Value {
		case "ocpu":
			ocpu = &shape.Products[i]
		case "memory":
			mem = &shape.Products[i]
		case "disk":
			disk = &shape.Products[i]
		}
	}
	if ocpu == nil {
		return nil
	}
	if shape.SubType.Value != "flexible" {
		size := oracleSize{name: shape.Name, ocpus: ocpu.Qty, memory: shape.BundleMemoryQty}
		if mem != nil && mem.Qty > 0 {
			size.memory = mem.Qty
		}
		if disk != nil {
			size.disk = disk.Qty
		}
		return []oracleSize{size}
	}
	if ocpu.Max == nil || mem == nil || mem.Max == nil {
		return nil
	}
	maxOCPUs, maxMem := *ocpu.Max, *mem.Max
	var counts []float64
	first := 1.0
	if disk != nil {
		first = 8
	}
	for n := first; n < maxOCPUs; n *= 2 {
		counts = append(counts, n)
	}
	counts = append(counts, maxOCPUs)

	sized := func(ocpus, memory, tb float64) oracleSize {
		return oracleSize{
			name:  fmt.Sprintf("%s-%s-%s", shape.Name, fmtQty(ocpus), fmtQty(memory)),
			ocpus: ocpus, memory: memory, disk: tb, flexible: true, maxOCPUs: maxOCPUs, maxMem: maxMem,
		}
	}
	var sizes []oracleSize
	for _, n := range counts {
		if disk != nil {
			if disk.Max == nil {
				return nil
			}
			sizes = append(sizes, sized(n, n*maxMem/maxOCPUs, n**disk.Max/maxOCPUs))
			continue
		}
		for _, perOCPU := range oracleMemoryPerOCPU {
			if memory := n * float64(perOCPU); memory <= maxMem {
				sizes = append(sizes, sized(n, memory, 0))
			}
		}
	}
	return sizes
}

func fmtQty(q float64) string { return strconv.FormatFloat(q, 'f', -1, 64) }

// readOracle turns a fetched Oracle file into a snapshot.
func readOracle(r io.Reader) (*snapshot, error) {
	var src oracleSource
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("decoding Oracle file: %w", err)
	}
	if src.Kind != oracleSourceKind {
		return nil, fmt.Errorf("not an Oracle file (kind %q)", src.Kind)
	}
	ts, err := parseTS(src.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("fetchedAt: %w", err)
	}
	parts := map[string]oraclePart{}
	windows, hasWindows := 0.0, false
	for _, p := range src.Parts {
		parts[p.PartNumber] = p
		if p.DisplayName == oracleWindowsPart {
			windows, hasWindows = p.usd()
		}
	}

	s := &snapshot{Offer: oracleOffer, TS: ts, Instances: map[string]*snapInstance{},
		Regions: map[string]map[string]string{oracleRegion: {"regionCode": oracleRegion, "locationType": "Global"}}}
	for _, shape := range src.Shapes {
		if shape.Status != "ACTIVE" {
			continue
		}
		s.Products++
		sizes := oracleSizes(shape)
		if len(sizes) == 0 {
			s.Unpriced++
			continue
		}
		for _, size := range sizes {
			hourly, rates, ok := oraclePrice(shape, size, parts)
			if !ok {
				s.Unpriced++
				continue
			}
			si := newSnapInstance()
			s.setOD(si, odKey{Region: oracleRegion, Variant: Variant{"Linux"}}, hourly)
			if hasWindows && shape.WindowsSupported {
				s.setOD(si, odKey{Region: oracleRegion, Variant: Variant{"Windows"}}, roundPrice(hourly+windows*size.ocpus))
			}
			attrs := map[string]string{
				"shape": shape.Name, "productFamily": "Compute", "shapeType": strings.ToUpper(shape.ShapeType.Value),
				"processor": shape.ProcessorType.Value, "ocpus": fmtQty(size.ocpus), "memoryInGb": fmtQty(size.memory),
				"description": shape.Description, "localDisk": shape.LocalDisk,
				"allowPreemptible": strconv.FormatBool(shape.AllowPreemptible), "allowBurstable": strconv.FormatBool(shape.AllowBurstable),
			}
			if size.disk > 0 {
				attrs["nvmeInTb"] = fmtQty(size.disk)
			}
			if shape.GpuQty != nil {
				attrs["gpuCount"] = fmtQty(*shape.GpuQty)
			}
			if shape.GpuMemoryQty != nil {
				attrs["gpuMemoryInGb"] = fmtQty(*shape.GpuMemoryQty)
			}
			if size.flexible {
				attrs["flexible"] = "true"
				attrs["maxOcpus"] = fmtQty(size.maxOCPUs)
				attrs["maxMemoryInGb"] = fmtQty(size.maxMem)
				for k, v := range rates {
					attrs[k] = strconv.FormatFloat(v, 'f', -1, 64)
				}
			}
			si.Attrs[""] = attrs
			s.Instances[size.name] = si
		}
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("Oracle file has no prices")
	}
	return s, nil
}

// oraclePrice adds up a shape's parts. A GPU shape lists its GPU part with its OCPU count as the quantity, so
// parts billed per GPU are multiplied by the GPU count instead.
func oraclePrice(shape oracleShape, size oracleSize, parts map[string]oraclePart) (float64, map[string]float64, bool) {
	total := 0.0
	rates := map[string]float64{}
	for _, sp := range shape.Products {
		part, ok := parts[sp.PartNumber]
		if !ok {
			return 0, nil, false
		}
		rate, ok := part.usd()
		if !ok {
			return 0, nil, false
		}
		qty := sp.Qty
		switch {
		case strings.Contains(part.MetricName, "GPU"):
			if shape.GpuQty == nil {
				return 0, nil, false
			}
			qty = *shape.GpuQty
		case sp.Type.Value == "ocpu":
			qty = size.ocpus
			rates["ocpuPrice"] = rate
		case sp.Type.Value == "memory":
			qty = size.memory
			rates["memoryGbPrice"] = rate
		case sp.Type.Value == "disk":
			qty = size.disk
			rates["nvmeTbPrice"] = rate
		}
		total += qty * rate
	}
	return roundPrice(total), rates, true
}
