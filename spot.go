package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// spotOffer is the offer code spot bundles record. AWS has no price list for spot prices, so it is not a real
// AWS offer code; the prices come from the feed behind AWS's Spot pricing page instead (see spot.sh).
const spotOffer = "AmazonEC2Spot"

// spotOS names the feed's price columns the way EC2 price lists spell operatingSystem.
var spotOS = map[string]string{"linux": "Linux", "mswin": "Windows"}

var (
	// localZone matches a Local Zone group's code: its parent region's code, then a city and a number.
	localZone = regexp.MustCompile(`^[a-z]{2}-[a-z]+-\d+-[a-z]+-\d+$`)
	// wavelengthZone matches a Wavelength Zone group's code, such as us-east-1-wl1.
	wavelengthZone = regexp.MustCompile(`-wl\d+(-|$)`)
)

// spotRegionAttrs are the regions table's attributes for a spot location, which the feed and spot price
// history name by code. They match what price lists give their locations, so every bundle's regions table
// has regionCode and locationType.
func spotRegionAttrs(code string) map[string]string {
	kind := "AWS Region"
	switch {
	case wavelengthZone.MatchString(code):
		kind = "AWS Wavelength Zone"
	case localZone.MatchString(code):
		kind = "AWS Local Zone"
	}
	return map[string]string{"regionCode": code, "locationType": kind}
}

type spotFeed struct {
	Config struct {
		Regions []struct {
			Region        string `json:"region"`
			InstanceTypes []struct {
				Sizes []struct {
					Size         string `json:"size"`
					ValueColumns []struct {
						Name   string            `json:"name"`
						Prices map[string]string `json:"prices"`
					} `json:"valueColumns"`
				} `json:"sizes"`
			} `json:"instanceTypes"`
		} `json:"regions"`
	} `json:"config"`
}

// isSpotFeed reports whether the file starting with head is a spot price feed rather than an AWS price list.
// The feed starts with "vers" and "config"; price lists start with "formatVersion" and "offerCode".
func isSpotFeed(head []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(head))
	if err := expectDelim(dec, '{'); err != nil {
		return false
	}
	t, err := dec.Token()
	return err == nil && (t == "vers" || t == "config")
}

// readSpot reads AWS's spot price feed. The feed has no publication date, so ts says when it was published.
// Its locations are region codes (us-east-1), not the location names (US East (N. Virginia)) price lists use;
// the regions table gives each its own code as regionCode, and its locationType.
func readSpot(r io.Reader, ts int64) (*snapshot, error) {
	var feed spotFeed
	if err := json.NewDecoder(bufio.NewReaderSize(r, 4<<20)).Decode(&feed); err != nil {
		return nil, fmt.Errorf("decoding spot price feed: %w", err)
	}
	if len(feed.Config.Regions) == 0 {
		return nil, fmt.Errorf("spot price feed has no regions")
	}
	s := &snapshot{Offer: spotOffer, TS: ts, Instances: map[string]*snapInstance{}, Regions: map[string]map[string]string{}}
	seen := map[[2]string]bool{}
	for _, reg := range feed.Config.Regions {
		if reg.Region == "" {
			return nil, fmt.Errorf("spot price feed has a region with no name")
		}
		s.Regions[reg.Region] = spotRegionAttrs(reg.Region)
		// The feed lists some instance types twice in a region, under the same category with the same prices.
		for _, group := range reg.InstanceTypes {
			for _, size := range group.Sizes {
				if size.Size == "" {
					return nil, fmt.Errorf("spot price feed has an instance type with no name in %s", reg.Region)
				}
				si := s.Instances[size.Size]
				if si == nil {
					si = &snapInstance{Attrs: map[string]map[string]string{}, OD: map[odKey]odPrice{}, RS: map[rsKey]rsPrice{}}
					s.Instances[size.Size] = si
				}
				if !seen[[2]string{reg.Region, size.Size}] {
					seen[[2]string{reg.Region, size.Size}] = true
					s.Products++
				}
				for _, col := range size.ValueColumns {
					usd, ok := col.Prices["USD"]
					// "N/A*" means the instance type isn't sold as spot with that operating system in the region.
					if !ok || strings.HasPrefix(usd, "N/A") {
						continue
					}
					p, err := strconv.ParseFloat(usd, 64)
					if err != nil {
						return nil, fmt.Errorf("spot price for %s %s in %s: bad price %q", size.Size, col.Name, reg.Region, usd)
					}
					os := col.Name
					if name, ok := spotOS[os]; ok {
						os = name
					}
					k := odKey{Region: reg.Region, Variant: Variant{os}}
					if prev, dup := si.OD[k]; dup && prev.Price != p {
						s.Collisions++
					}
					si.OD[k] = odPrice{Eff: ts, Price: p}
				}
			}
		}
	}
	// An instance type listed only with N/A prices has nothing to store.
	for name, si := range s.Instances {
		if len(si.OD) == 0 {
			delete(s.Instances, name)
		}
	}
	if len(s.Instances) == 0 {
		return nil, fmt.Errorf("spot price feed has no prices")
	}
	return s, nil
}
