package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"
)

// IBM Cloud's Global Catalog lists every Virtual Server for VPC profile, and each profile names the pricing
// plans it is billed under, whose prices are published per region. None of it needs a key, and it only has
// current prices, so `fetch ibm` saves one read of them, with when it was read. Each price keeps the date IBM
// says it took effect.
//
// Newer profiles have their own price in their own plan. Older ones share a plan, whose per-vCPU and per-GB rates
// don't add up to what IBM charges (bx2-2x8 would be $0.0916, not $0.101, and GPU profiles a third of their
// price), so their price is the one IBM gives per profile for an hourly reservation. That is an approximation: see
// the README for how close it is.

const ibmCatalogURL = "https://globalcatalog.cloud.ibm.com/api/v1"

const ibmSourceKind = "ibm-vpc-catalog"

const ibmOnDemand = "On-Demand"

// ibmRegionNames are the cities of IBM Cloud's multizone regions. Others just have no name.
var ibmRegionNames = map[string]string{
	"au-syd":   "Sydney",
	"br-sao":   "São Paulo",
	"ca-mon":   "Montreal",
	"ca-tor":   "Toronto",
	"eu-de":    "Frankfurt",
	"eu-es":    "Madrid",
	"eu-fr2":   "Paris",
	"eu-gb":    "London",
	"in-che":   "Chennai",
	"in-mum":   "Mumbai",
	"jp-osa":   "Osaka",
	"jp-tok":   "Tokyo",
	"us-east":  "Washington DC",
	"us-south": "Dallas",
}

// ibmHiddenConfig are the profile settings that aren't specs.
var ibmHiddenConfig = map[string]bool{"bss_info": true, "allowed_profile_classes": true, "provisioning_timeout_seconds": true}

// ibmKeepMetric picks the metrics a profile's price can use; the rest are operating system and software licenses.
var ibmKeepMetric = regexp.MustCompile(`^part-is\.(instance-hours-|spot-|burstable\d+-|reservation-hours-)`)

// ibmPriceOrigin marks a plan's current prices. A shared plan also lists older per-profile deployments in each
// region, under other ids, with prices that don't match what IBM charges.
const ibmPriceOrigin = "pricing_catalog"

type ibmDeployment struct {
	Type string `json:"type"`
	Plan string `json:"plan"`
}

type ibmProfile struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"displayName,omitempty"`
	Family      string          `json:"family,omitempty"`
	Generation  string          `json:"generation,omitempty"`
	Regions     []string        `json:"regions,omitempty"`
	Config      map[string]any  `json:"config,omitempty"`
	Deployments []ibmDeployment `json:"deployments"`
}

// ibmMetric is one price of a plan in a region: Price for every Quantity units, in USD.
type ibmMetric struct {
	ID       string  `json:"id"`
	Quantity float64 `json:"quantity"`
	Price    float64 `json:"price"`
}

func (m ibmMetric) rate() float64 {
	if m.Quantity > 0 {
		return m.Price / m.Quantity
	}
	return m.Price
}

type ibmPlanRegion struct {
	Region        string      `json:"region"`
	EffectiveFrom string      `json:"effectiveFrom,omitempty"`
	Metrics       []ibmMetric `json:"metrics"`
}

type ibmSource struct {
	Kind      string                     `json:"kind"`
	FetchedAt string                     `json:"fetchedAt"`
	Profiles  []ibmProfile               `json:"profiles"`
	Plans     map[string][]ibmPlanRegion `json:"plans"`
	// MissingPlans are plans the catalog refuses to show. It keeps some private, and a profile that needs one has
	// no price here.
	MissingPlans []string `json:"missingPlans,omitempty"`
}

// fetchIBM reads every instance profile and the prices of the plans they use, 8 plans at a time.
func fetchIBM(ctx context.Context, client *http.Client, base string) (*ibmSource, error) {
	fetched := time.Now().UTC()
	profiles, err := fetchIBMProfiles(ctx, client, base)
	if err != nil {
		return nil, err
	}
	plans := map[string]bool{}
	for _, p := range profiles {
		for _, d := range p.Deployments {
			plans[d.Plan] = true
		}
	}
	src := &ibmSource{Kind: ibmSourceKind, FetchedAt: fetched.Format(time.RFC3339), Profiles: profiles, Plans: map[string][]ibmPlanRegion{}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, 8)
	)
	for id := range plans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			regions, err := fetchIBMPlan(ctx, client, base, id)
			mu.Lock()
			defer mu.Unlock()
			var status *httpStatusError
			switch {
			case errors.As(err, &status) && (status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden || status.Code == http.StatusNotFound):
				src.MissingPlans = append(src.MissingPlans, id)
			case err != nil:
				if firstErr == nil {
					firstErr = fmt.Errorf("IBM plan %s: %w", id, err)
					cancel()
				}
			default:
				src.Plans[id] = regions
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	slices.Sort(src.MissingPlans)
	return src, nil
}

// fetchIBMProfiles lists every instance profile in the catalog, page by page.
func fetchIBMProfiles(ctx context.Context, client *http.Client, base string) ([]ibmProfile, error) {
	var profiles []ibmProfile
	for offset := 0; ; {
		var page struct {
			Count     int `json:"count"`
			Resources []struct {
				Name       string `json:"name"`
				OverviewUI struct {
					En struct {
						DisplayName string `json:"display_name"`
					} `json:"en"`
				} `json:"overview_ui"`
				Metadata struct {
					Other struct {
						Geography struct {
							Regions []struct {
								Name string `json:"name"`
							} `json:"regions"`
						} `json:"geography"`
						Profile struct {
							Family        string         `json:"family"`
							Generation    string         `json:"generation"`
							DefaultConfig map[string]any `json:"default_config"`
							Measures      []struct {
								Deployments []ibmDeployment `json:"deployments"`
							} `json:"measures"`
						} `json:"profile"`
					} `json:"other"`
				} `json:"metadata"`
			} `json:"resources"`
		}
		q := url.Values{"q": {"kind:instance.profile"}, "include": {"*"}, "_limit": {"200"}, "_offset": {strconv.Itoa(offset)}}
		if err := getJSON(ctx, client, base+"?"+q.Encode(), &page); err != nil {
			return nil, fmt.Errorf("IBM catalog: %w", err)
		}
		for _, r := range page.Resources {
			meta := r.Metadata.Other
			p := ibmProfile{Name: r.Name, DisplayName: r.OverviewUI.En.DisplayName, Family: meta.Profile.Family, Generation: meta.Profile.Generation, Config: map[string]any{}}
			for k, v := range meta.Profile.DefaultConfig {
				if !ibmHiddenConfig[k] {
					p.Config[k] = v
				}
			}
			for _, reg := range meta.Geography.Regions {
				p.Regions = append(p.Regions, reg.Name)
			}
			for _, m := range meta.Profile.Measures {
				p.Deployments = append(p.Deployments, m.Deployments...)
			}
			profiles = append(profiles, p)
		}
		offset += len(page.Resources)
		if len(page.Resources) == 0 || offset >= page.Count {
			break
		}
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("IBM catalog lists no instance profiles")
	}
	return profiles, nil
}

// fetchIBMPlan reads a plan's USD prices in each region. Deployments in single zones are left out: they repeat
// the plan's base price, while the region's deployment has the price charged there.
func fetchIBMPlan(ctx context.Context, client *http.Client, base, id string) ([]ibmPlanRegion, error) {
	var regions []ibmPlanRegion
	for offset := 0; ; {
		var page struct {
			Count     int `json:"count"`
			Resources []struct {
				Location string `json:"deployment_location"`
				Region   string `json:"deployment_region"`
				From     string `json:"effective_from"`
				Origin   string `json:"origin"`
				Metrics  []struct {
					ID       string  `json:"metric_id"`
					Quantity float64 `json:"charge_unit_quantity"`
					Amounts  []struct {
						Country string `json:"country"`
						Prices  []struct {
							Price float64 `json:"price"`
						} `json:"prices"`
					} `json:"amounts"`
				} `json:"metrics"`
			} `json:"resources"`
		}
		u := fmt.Sprintf("%s/%s/pricing/deployment?_limit=50&_offset=%d", base, url.PathEscape(id), offset)
		if err := getJSON(ctx, client, u, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Resources {
			if r.Location == "" || r.Location != r.Region || r.Origin != ibmPriceOrigin {
				continue
			}
			pr := ibmPlanRegion{Region: r.Region, EffectiveFrom: r.From}
			for _, m := range r.Metrics {
				if !ibmKeepMetric.MatchString(m.ID) {
					continue
				}
				for _, a := range m.Amounts {
					if a.Country == "USA" && len(a.Prices) > 0 {
						pr.Metrics = append(pr.Metrics, ibmMetric{ID: m.ID, Quantity: m.Quantity, Price: a.Prices[0].Price})
					}
				}
			}
			regions = append(regions, pr)
		}
		offset += len(page.Resources)
		if len(page.Resources) == 0 || offset >= page.Count {
			return regions, nil
		}
	}
}

// ibmLeases names the reservation terms by the end of their metric's id.
var ibmLeases = map[string]string{"one-year": "1yr", "three-year": "3yr"}

// readIBM turns a fetched IBM file into a snapshot.
func readIBM(r io.Reader) (*snapshot, error) {
	var src ibmSource
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("decoding IBM file: %w", err)
	}
	if src.Kind != ibmSourceKind {
		return nil, fmt.Errorf("not an IBM file (kind %q)", src.Kind)
	}
	ts, err := parseTS(src.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("fetchedAt: %w", err)
	}

	type regionPrices struct {
		eff     int64
		metrics map[string]ibmMetric
	}
	plans := map[string]map[string]regionPrices{}
	for id, regions := range src.Plans {
		plans[id] = map[string]regionPrices{}
		for _, reg := range regions {
			eff := ts
			if reg.EffectiveFrom != "" {
				e, err := parseTS(reg.EffectiveFrom)
				if err != nil {
					return nil, fmt.Errorf("plan %s in %s: effectiveFrom: %w", id, reg.Region, err)
				}
				eff = min(e, ts) // a price the catalog lists is in effect now, whatever date it gives
			}
			metrics := make(map[string]ibmMetric, len(reg.Metrics))
			for _, m := range reg.Metrics {
				metrics[m.ID] = m
			}
			plans[id][reg.Region] = regionPrices{eff: eff, metrics: metrics}
		}
	}

	s := &snapshot{Offer: ibmOffer, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}}
	for _, p := range src.Profiles {
		s.Products++
		var instance, reservation *ibmDeployment
		for i, d := range p.Deployments {
			switch d.Type {
			case "multi-tenant":
				instance = &p.Deployments[i]
			case "reservation-one-year":
				reservation = &p.Deployments[i]
			}
		}
		if instance == nil {
			s.Unpriced++ // the oldest profiles are listed, but no longer have a plan
			continue
		}
		si := newSnapInstance()
		for region, prices := range plans[instance.Plan] {
			if len(p.Regions) > 0 && !slices.Contains(p.Regions, region) {
				continue
			}
			setOD := func(capacity string, price float64) {
				if price > 0 {
					s.setODAt(si, odKey{Region: region, Variant: Variant{capacity}}, roundPrice(price), prices.eff)
				}
			}
			if m, ok := prices.metrics["part-is.instance-hours-"+p.Name]; ok {
				setOD(ibmOnDemand, m.rate())
			}
			if m, ok := prices.metrics["part-is.spot-"+p.Name]; ok {
				setOD("Spot", m.rate())
			}
			for _, percent := range []string{"50", "25", "10"} {
				if m, ok := prices.metrics["part-is.burstable"+percent+"-"+p.Name]; ok {
					setOD("Burstable "+percent+"%", m.rate())
				}
			}
		}
		if reservation != nil {
			for region, prices := range plans[reservation.Plan] {
				if len(p.Regions) > 0 && !slices.Contains(p.Regions, region) {
					continue
				}
				if _, priced := si.OD[odKey{Region: region, Variant: Variant{ibmOnDemand}}]; !priced {
					if m, ok := prices.metrics["part-is.reservation-hours-"+p.Name+"-hourly"]; ok && m.rate() > 0 {
						s.setODAt(si, odKey{Region: region, Variant: Variant{ibmOnDemand}}, roundPrice(m.rate()), prices.eff)
					}
				}
				for suffix, lease := range ibmLeases {
					m, ok := prices.metrics["part-is.reservation-hours-"+p.Name+"-"+suffix]
					if !ok || m.rate() <= 0 {
						continue
					}
					k := rsKey{Region: region, Variant: Variant{ibmOnDemand}, Term: Term{lease, "Reserved", "No Upfront"}}
					s.setRSAt(si, k, roundPrice(m.rate()), 0, prices.eff)
				}
			}
		}
		if len(si.OD) == 0 {
			s.Unpriced++
			continue
		}
		s.Instances[p.Name] = si

		attrs := map[string]string{"productFamily": "Virtual Server for VPC"}
		flatten("", p.Config, attrs)
		if p.DisplayName != "" {
			attrs["displayName"] = p.DisplayName
		}
		if p.Family != "" {
			attrs["family"] = p.Family
		}
		if p.Generation != "" {
			attrs["generation"] = p.Generation
		}
		si.Attrs[""] = attrs
		for region := range regionsOf(si) {
			if s.Regions[region] == nil {
				s.Regions[region] = map[string]string{"regionCode": region, "locationType": "IBM Cloud Region"}
				if n, ok := ibmRegionNames[region]; ok {
					s.Regions[region]["name"] = n
				}
			}
		}
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("IBM file has no prices")
	}
	return s, nil
}
