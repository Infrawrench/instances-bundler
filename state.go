package main

import "sort"

// The decoding rules: for each key, the row with the newest timestamp wins (later rows win ties).
// Regional attribute overrides keep applying even after a newer global change to the same key.

func (inst *Instance) attrState() (global map[string]*string, regional map[string]map[string]*string) {
	global = map[string]*string{}
	for k, v := range inst.Attrs {
		global[k] = &v
	}
	regional = map[string]map[string]*string{}
	changes := append([]AttrChange(nil), inst.AttrChanges...)
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].TS < changes[j].TS })
	for _, c := range changes {
		if c.Region == "" {
			global[c.Key] = c.Value
			continue
		}
		if regional[c.Region] == nil {
			regional[c.Region] = map[string]*string{}
		}
		regional[c.Region][c.Key] = c.Value
	}
	return global, regional
}

// AttrsIn returns the instance's current attributes in one region.
func (inst *Instance) AttrsIn(region string) map[string]string {
	global, regional := inst.attrState()
	out := map[string]string{}
	for k, v := range global {
		if v != nil {
			out[k] = *v
		}
	}
	for k, v := range regional[region] {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = *v
		}
	}
	return out
}

func (inst *Instance) latestOD() map[odKey]ODRow {
	m := make(map[odKey]ODRow, len(inst.OD))
	for _, r := range inst.OD {
		k := odKey{r.Region, r.Variant}
		if prev, ok := m[k]; !ok || r.TS >= prev.TS {
			m[k] = r
		}
	}
	return m
}

func (inst *Instance) latestRS() map[rsKey]RSRow {
	m := make(map[rsKey]RSRow, len(inst.RS))
	for _, r := range inst.RS {
		k := rsKey{r.Region, r.Variant, r.Term}
		if prev, ok := m[k]; !ok || r.TS >= prev.TS {
			m[k] = r
		}
	}
	return m
}

func (inst *Instance) price(r ODRow) float64 {
	if r.Price == nil {
		return inst.BasePrice
	}
	return *r.Price
}

// LiveOD returns the current on-demand price of every key still offered.
func (inst *Instance) LiveOD() map[odKey]float64 {
	out := map[odKey]float64{}
	for k, r := range inst.latestOD() {
		if !r.Removed {
			out[k] = inst.price(r)
		}
	}
	return out
}

// LiveRS returns the current [hourly, upfront] reserved price of every key still offered.
func (inst *Instance) LiveRS() map[rsKey][2]float64 {
	out := map[rsKey][2]float64{}
	for k, r := range inst.latestRS() {
		if !r.Removed {
			out[k] = [2]float64{r.Hourly, r.Upfront}
		}
	}
	return out
}
