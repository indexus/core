package protocol

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func FuzzACT2_IndependentComponentActivation(f *testing.F) {
	l1, _ := tree.NewZoneKey(demoCol, "aa")
	l3, _ := tree.NewZoneKey(demoCol, "ba")
	d, _ := tree.DecodeName("0123456789ABCDEF")
	f.Add(d.Bytes(), l1.ID.Bytes(), l3.ID.Bytes(), l1.ID.Bytes(), l3.ID.Bytes())
	f.Fuzz(func(t *testing.T, donor, a, b, za, zb []byte) {
		da, ja, jb := tree.FromSlice(donor), tree.FromSlice(a), tree.FromSlice(b)
		if da.IsZero() || ja.IsZero() || jb.IsZero() || da == ja || da == jb || ja == jb {
			return
		}
		old := tree.NewView(1)
		if err := old.Add("D", da); err != nil {
			return
		}
		neu, err := tree.Extend(old, tree.VNodeDescriptor{ID: ja, Name: "J1"}, tree.VNodeDescriptor{ID: jb, Name: "J2"})
		if err != nil {
			return
		}
		zones := []tree.ZoneKey{
			{Collection: demoCol, Location: "aa", ID: tree.FromSlice(za)},
			{Collection: demoCol, Location: "ba", ID: tree.FromSlice(zb)},
		}
		if zones[0].ID == zones[1].ID {
			zones = zones[:1]
		}
		comps, err := BuildActivationComponents(old, neu, zones)
		if err != nil {
			t.Fatal(err)
		}
		plan := map[tree.ID]struct{}{}
		for _, z := range changedBy(old, neu, zones) {
			plan[z.ID] = struct{}{}
		}
		seen := map[tree.ID]struct{}{}
		for _, c := range comps {
			for _, z := range changedBy(old, c.NewView, zones) {
				if _, ok := seen[z.ID]; ok {
					t.Fatalf("zone in two components")
				}
				seen[z.ID] = struct{}{}
				if c.NewView.Owner(z.ID) != neu.Owner(z.ID) {
					t.Fatal("component left the validated owner")
				}
			}
		}
		if len(seen) != len(plan) {
			t.Fatalf("covered %d want %d", len(seen), len(plan))
		}
	})
}

func changedBy(old, proposed *tree.View, zones []tree.ZoneKey) []tree.ZoneKey {
	var out []tree.ZoneKey
	for _, z := range zones {
		if old.Owner(z.ID) != proposed.Owner(z.ID) {
			out = append(out, z)
		}
	}
	return out
}
