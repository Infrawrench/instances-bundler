package main

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

type AppendStats struct {
	Created, Restored, Deleted  int
	AttrChanges, ODRows, RSRows int
	RegionChanges               int
}

// errNotNewer is returned when a pricing file is not newer than the bundle's latest one.
var errNotNewer = errors.New("pricing file is not newer than the bundle")

// Append records a newer pricing file, adding rows only where its state differs from the bundle's current state.
func (b *Bundle) Append(s *snapshot) (AppendStats, error) {
	var st AppendStats
	if b.Offer == "" {
		b.Offer = s.Offer
	} else if b.Offer != s.Offer {
		return st, fmt.Errorf("pricing file is for %s but the bundle is for %s", s.Offer, b.Offer)
	}
	if n := len(b.Snapshots); n > 0 && s.TS <= b.Snapshots[n-1] {
		return st, fmt.Errorf("%w: file is from %s, bundle already has %s; append files oldest first",
			errNotNewer, fmtTS(s.TS), fmtTS(b.Snapshots[n-1]))
	}
	b.Snapshots = append(b.Snapshots, s.TS)
	st.RegionChanges = b.mergeRegions(s)

	for _, name := range slices.Sorted(maps.Keys(s.Instances)) {
		si := s.Instances[name]
		inst, ok := b.Instances[name]
		if !ok {
			inst = &Instance{Attrs: baseAttrs(si), BasePrice: basePrice(si), CreatedAt: s.TS}
			b.Instances[name] = inst
			st.Created++
		} else if inst.DeletedAt != nil {
			inst.DeletedAt = nil
			st.Restored++
		}
		st.AttrChanges += inst.mergeAttrs(s.TS, si)
		st.ODRows += inst.mergeOD(s.TS, si)
		st.RSRows += inst.mergeRS(s.TS, si)
	}

	gone := &snapInstance{}
	for _, name := range slices.Sorted(maps.Keys(b.Instances)) {
		inst := b.Instances[name]
		if _, ok := s.Instances[name]; ok || inst.DeletedAt != nil {
			continue
		}
		ts := s.TS
		inst.DeletedAt = &ts
		st.Deleted++
		st.ODRows += inst.mergeOD(s.TS, gone)
		st.RSRows += inst.mergeRS(s.TS, gone)
	}
	return st, nil
}

// baseAttrs takes, for each key, the value most regions have.
func baseAttrs(si *snapInstance) map[string]string {
	out := map[string]string{}
	regions := slices.Sorted(maps.Keys(si.Attrs))
	for _, k := range attrKeys(si) {
		vals := make([]*string, len(regions))
		for i, r := range regions {
			vals[i] = lookup(si.Attrs[r], k)
		}
		if v := modeOf(vals); v != nil {
			out[k] = *v
		}
	}
	return out
}

// basePrice is the most common on-demand price, so the most rows can leave their price out.
func basePrice(si *snapInstance) float64 {
	counts := map[float64]int{}
	for _, p := range si.OD {
		counts[p.Price]++
	}
	best, n := 0.0, -1
	for p, c := range counts {
		if c > n || (c == n && p < best) {
			best, n = p, c
		}
	}
	return best
}

func (inst *Instance) mergeAttrs(ts int64, si *snapInstance) int {
	global, regional := inst.attrState()
	regions := slices.Sorted(maps.Keys(si.Attrs))
	keys := map[string]bool{}
	for k := range global {
		keys[k] = true
	}
	for _, k := range attrKeys(si) {
		keys[k] = true
	}
	for _, r := range regions {
		for k := range regional[r] {
			keys[k] = true
		}
	}

	added := 0
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		// A global change only reaches regions without their own override, so let those regions pick it.
		var free []*string
		for _, r := range regions {
			if _, ok := regional[r][k]; !ok {
				free = append(free, lookup(si.Attrs[r], k))
			}
		}
		if len(free) > 0 {
			if g := modeOf(free); !sameStr(g, global[k]) {
				inst.AttrChanges = append(inst.AttrChanges, AttrChange{TS: ts, Key: k, Value: g})
				global[k] = g
				added++
			}
		}
		for _, r := range regions {
			cur, ok := regional[r][k]
			if !ok {
				cur = global[k]
			}
			if want := lookup(si.Attrs[r], k); !sameStr(want, cur) {
				inst.AttrChanges = append(inst.AttrChanges, AttrChange{TS: ts, Key: k, Value: want, Region: r})
				added++
			}
		}
	}
	return added
}

func (inst *Instance) mergeOD(ts int64, si *snapInstance) int {
	cur := inst.latestOD()
	added := 0
	for _, k := range slices.SortedFunc(maps.Keys(si.OD), cmpODKey) {
		p := si.OD[k]
		prev, ok := cur[k]
		if ok && !prev.Removed && inst.price(prev) == p.Price {
			continue
		}
		row := ODRow{TS: rowTS(ts, p.Eff, prev.TS, ok), Region: k.Region, Variant: k.Variant}
		if p.Price != inst.BasePrice {
			row.Price = &p.Price
		}
		inst.OD = append(inst.OD, row)
		added++
	}
	for _, k := range slices.SortedFunc(maps.Keys(cur), cmpODKey) {
		if _, ok := si.OD[k]; ok || cur[k].Removed {
			continue
		}
		inst.OD = append(inst.OD, ODRow{TS: max(ts, cur[k].TS+1), Region: k.Region, Variant: k.Variant, Removed: true})
		added++
	}
	return added
}

func (inst *Instance) mergeRS(ts int64, si *snapInstance) int {
	cur := inst.latestRS()
	added := 0
	for _, k := range slices.SortedFunc(maps.Keys(si.RS), cmpRSKey) {
		p := si.RS[k]
		prev, ok := cur[k]
		if ok && !prev.Removed && prev.Hourly == p.Hourly && prev.Upfront == p.Upfront {
			continue
		}
		inst.RS = append(inst.RS, RSRow{TS: rowTS(ts, p.Eff, prev.TS, ok), Region: k.Region, Variant: k.Variant, Term: k.Term, Hourly: p.Hourly, Upfront: p.Upfront})
		added++
	}
	for _, k := range slices.SortedFunc(maps.Keys(cur), cmpRSKey) {
		if _, ok := si.RS[k]; ok || cur[k].Removed {
			continue
		}
		inst.RS = append(inst.RS, RSRow{TS: max(ts, cur[k].TS+1), Region: k.Region, Variant: k.Variant, Term: k.Term, Removed: true})
		added++
	}
	return added
}

// rowTS dates a price row by AWS's effectiveDate, unless that would not come after the key's previous row.
func rowTS(fileTS, eff, prevTS int64, hasPrev bool) int64 {
	if hasPrev && eff <= prevTS {
		return max(fileTS, prevTS+1)
	}
	return eff
}

func attrKeys(si *snapInstance) []string {
	keys := map[string]bool{}
	for _, attrs := range si.Attrs {
		for k := range attrs {
			keys[k] = true
		}
	}
	return slices.Sorted(maps.Keys(keys))
}

func lookup(m map[string]string, k string) *string {
	if v, ok := m[k]; ok {
		return &v
	}
	return nil
}

// modeOf returns the most common value, or nil when more regions lack the key than share any one value.
func modeOf(vals []*string) *string {
	counts := map[string]int{}
	absent := 0
	for _, v := range vals {
		if v == nil {
			absent++
		} else {
			counts[*v]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	best := mostCommon(counts)
	if absent > counts[best] {
		return nil
	}
	return &best
}

func sameStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func cmpODKey(a, b odKey) int {
	return cmp.Or(cmp.Compare(a.Region, b.Region), slices.Compare(a.Variant[:], b.Variant[:]))
}

func cmpRSKey(a, b rsKey) int {
	return cmp.Or(cmp.Compare(a.Region, b.Region), slices.Compare(a.Variant[:], b.Variant[:]), slices.Compare(a.Term[:], b.Term[:]))
}

func fmtTS(ts int64) string {
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}
