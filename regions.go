package main

import (
	"maps"
	"slices"
)

type liftStats struct {
	Rows      int // base attributes and change rows removed from instances
	Conflicts int // (region, attribute) pairs where instances disagreed; the most common value was kept
}

// liftRegionAttrs moves region attributes off instances into b.Regions, for bundles written before the regions table.
//
// A value is only taken where it is known to be current for that region: the instance is still sold there,
// or it has an explicit regional row. A base or global value says nothing about a region the instance
// left before the attribute existed.
func (b *Bundle) liftRegionAttrs() liftStats {
	var st liftStats
	votes := map[string]map[string]map[string]int{} // location -> key -> value -> instances
	vote := func(loc, k, v string) {
		if votes[loc] == nil {
			votes[loc] = map[string]map[string]int{}
		}
		if votes[loc][k] == nil {
			votes[loc][k] = map[string]int{}
		}
		votes[loc][k][v]++
	}

	for _, name := range slices.Sorted(maps.Keys(b.Instances)) {
		inst := b.Instances[name]
		if !inst.hasRegionAttrs() {
			continue
		}
		live := map[string]bool{}
		if inst.DeletedAt == nil {
			for k := range inst.LiveOD() {
				live[k.Region] = true
			}
			for k := range inst.LiveRS() {
				live[k.Region] = true
			}
		}
		for loc := range live {
			attrs := inst.AttrsIn(loc)
			for k := range regionAttrs {
				if v, ok := attrs[k]; ok {
					vote(loc, k, v)
				}
			}
		}
		_, regional := inst.attrState()
		for loc, kv := range regional {
			if live[loc] {
				continue
			}
			for k, v := range kv {
				if regionAttrs[k] && v != nil {
					vote(loc, k, *v)
				}
			}
		}

		for k := range inst.Attrs {
			if regionAttrs[k] {
				delete(inst.Attrs, k)
				st.Rows++
			}
		}
		kept := inst.AttrChanges[:0]
		for _, c := range inst.AttrChanges {
			if regionAttrs[c.Key] {
				st.Rows++
				continue
			}
			kept = append(kept, c)
		}
		inst.AttrChanges = kept
	}

	for loc, keys := range votes {
		if b.Regions[loc] == nil {
			b.Regions[loc] = map[string]string{}
		}
		for k, vals := range keys {
			if len(vals) > 1 {
				st.Conflicts++
			}
			if _, ok := b.Regions[loc][k]; !ok {
				b.Regions[loc][k] = mostCommon(vals)
			}
		}
	}
	return st
}

func (inst *Instance) hasRegionAttrs() bool {
	for k := range inst.Attrs {
		if regionAttrs[k] {
			return true
		}
	}
	for _, c := range inst.AttrChanges {
		if regionAttrs[c.Key] {
			return true
		}
	}
	return false
}

// mergeRegions records the file's region attributes; they describe the location now, so the newest value wins.
func (b *Bundle) mergeRegions(s *snapshot) int {
	if b.Regions == nil {
		b.Regions = map[string]map[string]string{}
	}
	changed := 0
	for loc, attrs := range s.Regions {
		if b.Regions[loc] == nil {
			b.Regions[loc] = map[string]string{}
		}
		for k, v := range attrs {
			if old, ok := b.Regions[loc][k]; !ok || old != v {
				b.Regions[loc][k] = v
				changed++
			}
		}
	}
	return changed
}
