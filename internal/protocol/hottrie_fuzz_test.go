package protocol

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func FuzzHotTrie_NoExternalSteal(f *testing.F) {
	f.Add([]byte{1}, []byte{128}, []byte{2}, []byte{3})
	f.Fuzz(func(t *testing.T, db, ub, za, zb []byte) {
		d, u := tree.FromSlice(db), tree.FromSlice(ub)
		if d.IsZero() || u.IsZero() || d == u {
			return
		}
		view := tree.NewView(1)
		if err := view.Add("D", d); err != nil {
			return
		}
		if err := view.Add("U", u); err != nil {
			return
		}
		z1 := tree.ZoneKey{Collection: "c", Location: "aa", ID: tree.FromSlice(za)}
		z2 := tree.ZoneKey{Collection: "c", Location: "ba", ID: tree.FromSlice(zb)}
		zones := []ZoneStats{
			{Zone: z1, Items: 10, WriteFast: 40},
			{Zone: z2, Items: 10, WriteFast: 30},
		}
		in := HotInput{Donor: d, Current: view, WritePressure: 80, TargetWrite: 20, Zones: zones}
		plan, err := PlanHotSplit(in)
		if err != nil {
			if view.Len() != 2 {
				t.Fatal("failed plan mutated V_active")
			}
			return
		}
		vp, err := ValidatePlan(view, plan, zones)
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range vp.Changes {
			if ch.OldOwner != d {
				t.Fatal("external steal")
			}
		}
		old := viewIDs(view)
		neu := viewIDs(vp.Next)
		for _, ch := range testoracle.OwnershipDiff(old, neu, []tree.ID{z1.ID, z2.ID}) {
			if ch.Old != d {
				t.Fatal("oracle saw external steal")
			}
		}
	})
}
