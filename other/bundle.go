package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
)

// Bundle is a bundle file with every index resolved back to its string; rows keep their full history.
// The file layout is instances-bundler's (see README.md), so the same readers work for every provider.
type Bundle struct {
	Offer         string // empty until the first snapshot is appended
	SourceVersion string // the provider's name for the latest snapshot, if it names them
	Snapshots     []int64
	Regions       map[string]map[string]string // location -> region attribute -> value
	Instances     map[string]*Instance
}

type Instance struct {
	Attrs       map[string]string
	AttrChanges []AttrChange
	BasePrice   float64
	OD          []ODRow
	RS          []RSRow
	CreatedAt   int64
	DeletedAt   *int64
}

// AttrChange sets Key to Value (nil removes it) from TS on. An empty Region means global.
type AttrChange struct {
	TS     int64
	Key    string
	Value  *string
	Region string
}

type ODRow struct {
	TS      int64
	Region  string
	Variant Variant
	Price   *float64 // nil means BasePrice
	Removed bool
}

type RSRow struct {
	TS              int64
	Region          string
	Variant         Variant
	Term            Term
	Hourly, Upfront float64
	Removed         bool
}

type bundleFile struct {
	Offer            string           `json:"offer"`
	SourceVersion    string           `json:"source_version,omitempty"`
	CompressionTable []string         `json:"compression_table"`
	Snapshots        []int64          `json:"snapshots"`
	Regions          [][]any          `json:"regions"`
	VariantKeys      []string         `json:"variant_keys"`
	Variants         [][]int          `json:"variants"`
	ReservedTerms    [][]int          `json:"reserved_terms"`
	Instances        map[string][]any `json:"instances"`
}

// Write encodes the bundle, ordering the compression table and lookup tables by use so common entries get short indices.
func (b *Bundle) Write(w io.Writer) error {
	offer, err := lookupOffer(b.Offer)
	if err != nil {
		return err
	}
	nkeys := len(offer.VariantKeys)
	strs := map[string]int{}
	variants := map[Variant]int{}
	terms := map[Term]int{}
	for loc, attrs := range b.Regions {
		strs[loc]++
		for k, v := range attrs {
			strs[k]++
			strs[v]++
		}
	}
	for _, inst := range b.Instances {
		for k, v := range inst.Attrs {
			strs[k]++
			strs[v]++
		}
		for _, c := range inst.AttrChanges {
			strs[c.Key]++
			if c.Value != nil {
				strs[*c.Value]++
			}
			if c.Region != "" {
				strs[c.Region]++
			}
		}
		for _, r := range inst.OD {
			strs[r.Region]++
			variants[r.Variant]++
		}
		for _, r := range inst.RS {
			strs[r.Region]++
			variants[r.Variant]++
			terms[r.Term]++
		}
	}
	for v := range variants {
		for _, s := range v[:nkeys] {
			strs[s]++
		}
	}
	for t := range terms {
		for _, s := range t {
			strs[s]++
		}
	}

	table := byUse(strs, cmp.Compare[string])
	vlist := byUse(variants, func(a, b Variant) int { return slices.Compare(a[:], b[:]) })
	tlist := byUse(terms, func(a, b Term) int { return slices.Compare(a[:], b[:]) })
	S, V, T := indexOf(table), indexOf(vlist), indexOf(tlist)

	out := bundleFile{
		Offer:            b.Offer,
		SourceVersion:    b.SourceVersion,
		CompressionTable: table,
		Snapshots:        b.Snapshots,
		Regions:          make([][]any, 0, len(b.Regions)),
		VariantKeys:      offer.VariantKeys,
		Variants:         make([][]int, len(vlist)),
		ReservedTerms:    make([][]int, len(tlist)),
		Instances:        make(map[string][]any, len(b.Instances)),
	}
	if out.Snapshots == nil {
		out.Snapshots = []int64{}
	}
	for _, loc := range slices.Sorted(maps.Keys(b.Regions)) {
		attrs := b.Regions[loc]
		pairs := make([][2]int, 0, len(attrs))
		for _, k := range slices.Sorted(maps.Keys(attrs)) {
			pairs = append(pairs, [2]int{S[k], S[attrs[k]]})
		}
		out.Regions = append(out.Regions, []any{S[loc], pairs})
	}
	for i, v := range vlist {
		out.Variants[i] = []int{}
		for _, s := range v[:nkeys] {
			out.Variants[i] = append(out.Variants[i], S[s])
		}
	}
	for i, t := range tlist {
		for _, s := range t {
			out.ReservedTerms[i] = append(out.ReservedTerms[i], S[s])
		}
	}
	for name, inst := range b.Instances {
		inst.sortRows()
		attrs := make([][2]int, 0, len(inst.Attrs))
		for _, k := range slices.Sorted(maps.Keys(inst.Attrs)) {
			attrs = append(attrs, [2]int{S[k], S[inst.Attrs[k]]})
		}
		changes := make([][]any, 0, len(inst.AttrChanges))
		for _, c := range inst.AttrChanges {
			row := []any{c.TS, S[c.Key], nil}
			if c.Value != nil {
				row[2] = S[*c.Value]
			}
			if c.Region != "" {
				row = append(row, S[c.Region])
			}
			changes = append(changes, row)
		}
		od := make([][]any, 0, len(inst.OD))
		for _, r := range inst.OD {
			row := []any{r.TS, S[r.Region], V[r.Variant]}
			switch {
			case r.Removed:
				row = append(row, nil)
			case r.Price != nil:
				row = append(row, *r.Price)
			}
			od = append(od, row)
		}
		rs := make([][]any, 0, len(inst.RS))
		for _, r := range inst.RS {
			row := []any{r.TS, S[r.Region], V[r.Variant], T[r.Term]}
			if r.Removed {
				row = append(row, nil)
			} else {
				row = append(row, r.Hourly)
				if r.Upfront != 0 {
					row = append(row, r.Upfront)
				}
			}
			rs = append(rs, row)
		}
		var deleted any
		if inst.DeletedAt != nil {
			deleted = *inst.DeletedAt
		}
		out.Instances[name] = []any{attrs, changes, inst.BasePrice, od, rs, inst.CreatedAt, deleted}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

func (inst *Instance) sortRows() {
	sort.SliceStable(inst.AttrChanges, func(i, j int) bool { return inst.AttrChanges[i].TS < inst.AttrChanges[j].TS })
	sort.SliceStable(inst.OD, func(i, j int) bool { return inst.OD[i].TS < inst.OD[j].TS })
	sort.SliceStable(inst.RS, func(i, j int) bool { return inst.RS[i].TS < inst.RS[j].TS })
}

func byUse[K comparable](counts map[K]int, tie func(a, b K) int) []K {
	keys := slices.Collect(maps.Keys(counts))
	slices.SortFunc(keys, func(a, b K) int {
		return cmp.Or(cmp.Compare(counts[b], counts[a]), tie(a, b))
	})
	return keys
}

func indexOf[K comparable](list []K) map[K]int {
	m := make(map[K]int, len(list))
	for i, k := range list {
		m[k] = i
	}
	return m
}

func ReadBundle(r io.Reader) (*Bundle, error) {
	var raw struct {
		Offer            string                       `json:"offer"`
		SourceVersion    string                       `json:"source_version"`
		CompressionTable []string                     `json:"compression_table"`
		Snapshots        []int64                      `json:"snapshots"`
		Regions          [][2]json.RawMessage         `json:"regions"`
		VariantKeys      []string                     `json:"variant_keys"`
		Variants         [][]int                      `json:"variants"`
		ReservedTerms    [][]int                      `json:"reserved_terms"`
		Instances        map[string][]json.RawMessage `json:"instances"`
	}
	if err := json.NewDecoder(bufio.NewReaderSize(r, 4<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding bundle: %w", err)
	}
	if raw.Offer == "" {
		return nil, fmt.Errorf("bundle has no offer; AWS bundles are instances-bundler's")
	}
	offer, err := lookupOffer(raw.Offer)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(raw.VariantKeys, offer.VariantKeys) {
		return nil, fmt.Errorf("bundle's variant keys %v differ from %s's %v", raw.VariantKeys, raw.Offer, offer.VariantKeys)
	}
	d := &bundleDecoder{table: raw.CompressionTable}
	for _, v := range raw.Variants {
		if len(v) != len(offer.VariantKeys) {
			return nil, fmt.Errorf("variant %v should have %d fields", v, len(offer.VariantKeys))
		}
		var out Variant
		for i, x := range v {
			out[i] = d.str(x)
		}
		d.variants = append(d.variants, out)
	}
	for _, t := range raw.ReservedTerms {
		if len(t) != len(Term{}) {
			return nil, fmt.Errorf("reserved term %v should have %d fields", t, len(Term{}))
		}
		var out Term
		for i, x := range t {
			out[i] = d.str(x)
		}
		d.terms = append(d.terms, out)
	}
	b := &Bundle{
		Offer:         raw.Offer,
		SourceVersion: raw.SourceVersion,
		Snapshots:     raw.Snapshots,
		Regions:       map[string]map[string]string{},
		Instances:     make(map[string]*Instance, len(raw.Instances)),
	}
	for _, entry := range raw.Regions {
		var loc int
		var pairs [][2]int
		if err := json.Unmarshal(entry[0], &loc); err != nil {
			return nil, fmt.Errorf("regions: %w", err)
		}
		if err := json.Unmarshal(entry[1], &pairs); err != nil {
			return nil, fmt.Errorf("regions: %w", err)
		}
		attrs := map[string]string{}
		for _, kv := range pairs {
			attrs[d.str(kv[0])] = d.str(kv[1])
		}
		b.Regions[d.str(loc)] = attrs
	}
	for name, parts := range raw.Instances {
		inst, err := d.instance(parts)
		if err != nil {
			return nil, fmt.Errorf("instance %s: %w", name, err)
		}
		b.Instances[name] = inst
	}
	if d.err != nil {
		return nil, d.err
	}
	return b, nil
}

type bundleDecoder struct {
	table    []string
	variants []Variant
	terms    []Term
	err      error
}

func (d *bundleDecoder) fail(format string, a ...any) {
	if d.err == nil {
		d.err = fmt.Errorf(format, a...)
	}
}

func (d *bundleDecoder) str(i int) string {
	if i < 0 || i >= len(d.table) {
		d.fail("string index %d out of range", i)
		return ""
	}
	return d.table[i]
}

func (d *bundleDecoder) variant(i int) Variant {
	if i < 0 || i >= len(d.variants) {
		d.fail("variant index %d out of range", i)
		return Variant{}
	}
	return d.variants[i]
}

func (d *bundleDecoder) term(i int) Term {
	if i < 0 || i >= len(d.terms) {
		d.fail("reserved term index %d out of range", i)
		return Term{}
	}
	return d.terms[i]
}

// field reads row[i]; nulls and missing trailing fields both come back as absent.
func field(row []*float64, i int) (float64, bool) {
	if i >= len(row) || row[i] == nil {
		return 0, false
	}
	return *row[i], true
}

func (d *bundleDecoder) need(row []*float64, i int) float64 {
	v, ok := field(row, i)
	if !ok {
		d.fail("row is missing field %d", i)
	}
	return v
}

func (d *bundleDecoder) instance(parts []json.RawMessage) (*Instance, error) {
	if len(parts) != 7 {
		return nil, fmt.Errorf("expected 7 fields, got %d", len(parts))
	}
	var attrs [][2]int
	var changes, od, rs [][]*float64
	inst := &Instance{Attrs: map[string]string{}}
	for i, dst := range []any{&attrs, &changes, &inst.BasePrice, &od, &rs, &inst.CreatedAt, &inst.DeletedAt} {
		if err := json.Unmarshal(parts[i], dst); err != nil {
			return nil, fmt.Errorf("field %d: %w", i, err)
		}
	}
	for _, kv := range attrs {
		inst.Attrs[d.str(kv[0])] = d.str(kv[1])
	}
	for _, row := range changes {
		c := AttrChange{TS: int64(d.need(row, 0)), Key: d.str(int(d.need(row, 1)))}
		if v, ok := field(row, 2); ok {
			s := d.str(int(v))
			c.Value = &s
		}
		if v, ok := field(row, 3); ok {
			c.Region = d.str(int(v))
		}
		inst.AttrChanges = append(inst.AttrChanges, c)
	}
	for _, row := range od {
		r := ODRow{TS: int64(d.need(row, 0)), Region: d.str(int(d.need(row, 1))), Variant: d.variant(int(d.need(row, 2)))}
		switch {
		case len(row) < 4:
		case row[3] == nil:
			r.Removed = true
		default:
			p := *row[3]
			r.Price = &p
		}
		inst.OD = append(inst.OD, r)
	}
	for _, row := range rs {
		r := RSRow{TS: int64(d.need(row, 0)), Region: d.str(int(d.need(row, 1))), Variant: d.variant(int(d.need(row, 2))), Term: d.term(int(d.need(row, 3)))}
		if h, ok := field(row, 4); ok {
			r.Hourly = h
			r.Upfront, _ = field(row, 5)
		} else {
			r.Removed = true
		}
		inst.RS = append(inst.RS, r)
	}
	return inst, d.err
}
