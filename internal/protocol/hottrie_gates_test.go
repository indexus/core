package protocol

import (
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/testoracle"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestHotTrie_NoExternalStealExhaustive8Bit(t *testing.T) {
	const bits uint8 = 8
	d := tree.ID(testoracle.Packed(1, bits))
	u := tree.ID(testoracle.Packed(200, bits))
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	g := Guard(view, d)
	var zones []ZoneStats
	var keys []tree.ID
	var forkable []tree.ID
	for k := uint64(0); k < 1<<bits; k++ {
		id := tree.ID(testoracle.Packed(k, bits))
		keys = append(keys, id)
		if id.IsZero() {
			zones = append(zones, ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "z", ID: id}})
			continue
		}
		w := 0.0
		if view.Owner(id) == d && id != d {
			if _, ok := tree.FirstDiffFrom(d, id, g); ok {
				forkable = append(forkable, id)
				if len(forkable) == 1 {
					w = 40
				}
			}
		}
		zones = append(zones, ZoneStats{
			Zone:      tree.ZoneKey{Collection: "c", Location: "z", ID: id},
			Items:     1,
			WriteFast: w,
		})
	}
	if len(forkable) == 0 {
		t.Fatal("no donor-owned forkable key in 8-bit space")
	}
	in := HotInput{Donor: d, Current: view, WritePressure: 40, TargetWrite: 10, Zones: zones}
	plan, err := PlanHotSplit(in)
	if err != nil {
		t.Fatal(err)
	}
	vp, err := ValidatePlan(view, plan, zones)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range testoracle.OwnershipDiff(viewIDs(view), viewIDs(vp.Next), keys) {
		if ch.Old != d {
			t.Fatalf("∀x owner change from %v not D", ch.Old)
		}
	}
}

func TestHotTrie_SafeAtomEscalates(t *testing.T) {
	TestSafeAtomEscalates(t)
}

func TestHotTrie_StopsAfterSufficientRelief(t *testing.T) {
	TestIncrementalReliefStops(t)
}

func TestHotTrie_WritePressureRanksByReliefPerETA(t *testing.T) {
	a := ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}, WriteFast: 25000, ETA: 10 * time.Second}
	b := ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "ab", ID: bitID(93)}, WriteFast: 17000, ETA: 300 * time.Millisecond}
	c := ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "ba", ID: bitID(92)}, WriteFast: 15000, ETA: 400 * time.Millisecond}
	zones := []ZoneStats{a, b, c}
	first := hottestReliefLeaf(zones, 0, true)
	if zones[first].Zone.Location != "ab" {
		t.Fatalf("first leaf %s want ab (B)", zones[first].Zone.Location)
	}
	rest := append([]ZoneStats{}, zones[:first]...)
	rest = append(rest, zones[first+1:]...)
	second := hottestReliefLeaf(rest, 0, true)
	if rest[second].Zone.Location != "ba" {
		t.Fatalf("second leaf %s want ba (C)", rest[second].Zone.Location)
	}
}

func TestHotTrie_DiskPressureRanksByDropPerETA(t *testing.T) {
	a := ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}, Items: 25000, ETA: 10 * time.Second}
	b := ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "ab", ID: bitID(93)}, Items: 8000, ETA: 300 * time.Millisecond}
	c := ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "ba", ID: bitID(92)}, Items: 9000, ETA: 400 * time.Millisecond}
	zones := []ZoneStats{a, b, c}
	first := hottestReliefLeaf(zones, 0, false)
	if zones[first].Zone.Location != "ab" {
		t.Fatalf("disk rank first %s want ab", zones[first].Zone.Location)
	}
}

func TestHotTrie_SafeAtomNoSideEffects(t *testing.T) {
	d := bitID(95)
	u := tree.SetBit(d, 10, 1)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: u, Name: "U"})
	digest := view.Digest
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: tree.SetBit(d, 5, 1)}
	plan, err := PlanHotSplit(HotInput{
		Donor: d, Current: view, WritePressure: 80, TargetWrite: 10,
		Zones: []ZoneStats{{Zone: z, WriteFast: 80}},
	})
	if !errors.Is(err, ErrSafeSplitInsufficient) {
		t.Fatalf("%v", err)
	}
	if len(plan.Add) != 0 {
		t.Fatal("insufficient split must not Add")
	}
	if view.Digest != digest {
		t.Fatal("V_active mutated")
	}
}

func TestRegionalAdd_BoundaryNeighborProtected(t *testing.T) {
	d := tree.SetBit(tree.ID{}, 10, 1)
	u := tree.SetBit(d, 4, 1)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	// These witnesses make the advertised prefix ownership-closed. Without
	// them, keys outside the common leading prefix can still belong to D/J.
	for bit := uint8(0); bit < 4; bit++ {
		id := tree.SetBit(d, bit, 1)
		if err := view.Add(tree.EncodeName(id), id); err != nil {
			t.Fatal(err)
		}
	}

	hot := tree.SetBit(d, 5, 1)
	outside := tree.SetBit(u, 20, 1)
	zones := []ZoneStats{
		{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: hot}, WriteFast: 80},
		{Zone: tree.ZoneKey{Collection: "c", Location: "zz", ID: outside}, WriteFast: 1},
	}
	plan, err := PlanRegionalAdd(HotInput{Donor: d, Current: view, WritePressure: 80, TargetWrite: 10, Zones: zones})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Region.Bits == 0 {
		t.Fatal("region prefix missing")
	}
	if plan.Region.Contains(u) {
		t.Fatal("outside neighbor sharing p[:-1] must be outside R")
	}
	vp, err := ValidatePlan(view, plan, zones)
	if err != nil {
		t.Fatal(err)
	}
	if view.Owner(outside) != vp.Next.Owner(outside) {
		t.Fatal("first differing bit before |p| failed to protect the exterior")
	}
	for _, ch := range vp.Changes {
		if !plan.Region.Contains(ch.OldOwner) {
			t.Fatal("XOR consequence leaked outside R")
		}
	}
}

func TestRegionalAdd_Exhaustive8Bit(t *testing.T) {
	const bits uint8 = 8
	d := tree.ID(testoracle.Packed(1, bits))
	u := tree.ID(testoracle.Packed(200, bits))
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	var zones []ZoneStats
	var keys []tree.ID
	hot := tree.ID(testoracle.Packed(3, bits))
	for k := uint64(0); k < 1<<bits; k++ {
		id := tree.ID(testoracle.Packed(k, bits))
		w := 1.0
		if id == hot {
			w = 100
		}
		zones = append(zones, ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "z", ID: id}, WriteFast: w})
		keys = append(keys, id)
	}
	digest := view.Digest
	plan, err := PlanRegionalAdd(HotInput{Donor: d, Current: view, WritePressure: 400, TargetWrite: 10, Zones: zones})
	if view.Digest != digest {
		t.Fatal("planner mutated the live view")
	}
	if err != nil {
		if !errors.Is(err, ErrSafeSplitInsufficient) && !errors.Is(err, ErrAtomicOverload) {
			t.Fatal(err)
		}
		return
	}
	vp, err := ValidatePlan(view, plan, zones)
	if err != nil {
		t.Fatal(err)
	}
	for _, add := range plan.Add {
		if !plan.Region.Contains(add.ID) {
			t.Fatal("added id outside R")
		}
	}
	for _, ch := range testoracle.OwnershipDiff(viewIDs(view), viewIDs(vp.Next), keys) {
		if !plan.Region.Contains(ch.Old) || !plan.Region.Contains(ch.Zone) {
			t.Fatalf("key outside region changed owner: old=%v zone=%v", ch.Old, ch.Zone)
		}
	}
}

func FuzzRegionalAdd_NoExternalRegionChange(f *testing.F) {
	f.Add([]byte{1}, []byte{2}, []byte{3}, []byte{4})
	f.Fuzz(func(t *testing.T, db, ub, za, zb []byte) {
		d, u := tree.FromSlice(db), tree.FromSlice(ub)
		if d.IsZero() || u.IsZero() || d == u {
			return
		}
		view := tree.NewView(1)
		if view.Add("D", d) != nil || view.Add("U", u) != nil {
			return
		}
		zones := []ZoneStats{
			{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: tree.FromSlice(za)}, WriteFast: 40},
			{Zone: tree.ZoneKey{Collection: "c", Location: "ba", ID: tree.FromSlice(zb)}, WriteFast: 30},
		}
		plan, err := PlanRegionalAdd(HotInput{Donor: d, Current: view, WritePressure: 80, TargetWrite: 10, Zones: zones})
		if err != nil {
			return
		}
		vp, err := ValidatePlan(view, plan, zones)
		if err != nil {
			t.Fatal(err)
		}
		for _, add := range plan.Add {
			if !plan.Region.Contains(add.ID) {
				t.Fatal("added id outside R")
			}
		}
		for _, ch := range vp.Changes {
			if !plan.Region.Contains(ch.OldOwner) || !plan.Region.Contains(ch.Zone.ID) {
				t.Fatal("change outside R")
			}
		}
	})
}
