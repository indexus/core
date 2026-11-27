package protocol

import (
	"errors"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func bitID(bit uint8) tree.ID {
	return tree.SetBit(tree.ID{}, bit, 1)
}

func mustView(t *testing.T, pairs ...tree.VNodeDescriptor) *tree.View {
	t.Helper()
	v := tree.NewView(1)
	for _, d := range pairs {
		if err := v.Add(d.Name, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

func TestValidatePlanIgnoresPlannerChanges(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}
	j, err := tree.HotSplitAddress(d, Guard(view, d))
	if err != nil {
		t.Fatal(err)
	}
	plan := TopologyPlan{
		BaseDigest: view.Digest,
		Donor:      d,
		Add:        []tree.VNodeDescriptor{{ID: j, Name: "J"}},
		Reason:     ReasonHotSplit,
		Changes: []ZoneChange{{
			Zone:     z,
			OldOwner: u, // lie
			NewOwner: u,
		}},
	}
	got, err := ValidatePlan(view, plan, []ZoneStats{{Zone: z}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) == 1 && got.Changes[0].OldOwner == u {
		t.Fatal("ValidatePlan trusted the planner's owners")
	}
	for _, ch := range got.Changes {
		if ch.OldOwner != d {
			t.Fatalf("donor-only broken: old=%v", ch.OldOwner)
		}
	}
}

func TestHotTrieSafeSpawn(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	hot := tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}
	cold := tree.ZoneKey{Collection: "c", Location: "ba", ID: u}
	in := HotInput{
		Donor:         d,
		Current:       view,
		WritePressure: 80,
		TargetWrite:   50,
		Zones: []ZoneStats{
			{Zone: hot, Items: 100, WriteFast: 40},
			{Zone: cold, Items: 10, WriteFast: 1},
		},
	}
	plan, err := PlanHotSplit(in)
	if err != nil {
		t.Fatal(err)
	}
	vp, err := ValidatePlan(view, plan, in.Zones)
	if err != nil {
		t.Fatal(err)
	}
	if len(vp.Changes) == 0 {
		t.Fatal("expected ownership change")
	}
	for _, ch := range vp.Changes {
		if ch.OldOwner != d {
			t.Fatalf("external steal from %v", ch.OldOwner)
		}
		if view.Owner(ch.Zone.ID) != d {
			t.Fatal("changed a zone D did not own")
		}
	}
}

func TestHOT2_ImpossibleSplitHasNoSideEffect(t *testing.T) {
	d := bitID(95)
	u := tree.SetBit(d, 10, 1)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	digest, gen, n := view.Digest, view.Generation, view.Len()
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: tree.SetBit(d, 5, 1)}
	plan, err := PlanHotSplit(HotInput{
		Donor:         d,
		Current:       view,
		WritePressure: 80,
		TargetWrite:   10,
		Zones:         []ZoneStats{{Zone: z, Items: 50, WriteFast: 80}},
	})
	if !errors.Is(err, ErrSafeSplitInsufficient) {
		t.Fatalf("want ErrSafeSplitInsufficient, got %v", err)
	}
	if len(plan.Add) != 0 || len(plan.Remove) != 0 || plan.Reason != ReasonNone {
		t.Fatalf("refused split still produced %+v", plan)
	}
	if view.Digest != digest || view.Generation != gen || view.Len() != n {
		t.Fatal("refused split mutated V_active")
	}
}

func TestSafeAtomEscalates(t *testing.T) {
	d := bitID(95)
	u := tree.SetBit(d, 10, 1)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: tree.SetBit(d, 5, 1)}
	in := HotInput{
		Donor:         d,
		Current:       view,
		WritePressure: 80,
		TargetWrite:   10,
		Zones:         []ZoneStats{{Zone: z, Items: 50, WriteFast: 80}},
	}
	_, err := PlanHotSplit(in)
	if !errors.Is(err, ErrSafeSplitInsufficient) {
		t.Fatalf("want ErrSafeSplitInsufficient, got %v", err)
	}
	plan, err := PlanRegionalAdd(in)
	if err != nil {
		t.Fatal(err)
	}
	vp, err := ValidatePlan(view, plan, in.Zones)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range vp.Changes {
		if !plan.Region.Contains(ch.OldOwner) {
			t.Fatal("regional add stole outside the region")
		}
	}
}

func TestAtomicOverload(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}
	_, err := PlanHotSplit(HotInput{
		Donor:         d,
		Current:       view,
		WritePressure: 200,
		TargetWrite:   50,
		AtomicWrite:   100,
		Zones:         []ZoneStats{{Zone: z, WriteFast: 150}},
	})
	if !errors.Is(err, ErrAtomicOverload) {
		t.Fatalf("got %v", err)
	}
}

func TestIncrementalReliefStops(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	l1 := tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}
	l3 := tree.ZoneKey{Collection: "c", Location: "ba", ID: bitID(93)}
	in := HotInput{
		Donor:         d,
		Current:       view,
		WritePressure: 75,
		TargetWrite:   50,
		DrainRate:     100,
		Zones: []ZoneStats{
			{Zone: l1, Items: 22, WriteFast: 25},
			{Zone: l3, Items: 19, WriteFast: 22},
			{Zone: tree.ZoneKey{Collection: "c", Location: "ab", ID: bitID(92)}, Items: 16, WriteFast: 17},
			{Zone: tree.ZoneKey{Collection: "c", Location: "bb", ID: bitID(91)}, Items: 11, WriteFast: 8},
		},
	}
	plan, err := PlanHotSplit(in)
	if err != nil {
		t.Fatal(err)
	}
	// HOT_SPLIT admits exactly one guarded fork. It must stop as soon as
	// that fork provides sufficient relief, and the resulting plan must pass
	// the same complete-domain validation as an executable runtime plan.
	if len(plan.Add) != 1 {
		t.Fatalf("want one sufficient fork, got %d", len(plan.Add))
	}
	validated, err := ValidatePlan(view, plan, in.Zones)
	if err != nil {
		t.Fatal(err)
	}
	relief := 0.0
	for _, change := range validated.Changes {
		relief += change.WriteFast
	}
	if in.WritePressure-relief > in.TargetWrite {
		t.Fatalf("insufficient relief: pressure=%v relief=%v", in.WritePressure, relief)
	}
	// An additional unit of pressure would require multiple independent
	// forks here. Refuse rather than returning an unexecutable multi-ADD
	// HOT_SPLIT; the controller can select a regional plan instead.
	in.WritePressure = 76
	if refused, err := PlanHotSplit(in); !errors.Is(err, ErrSafeSplitInsufficient) || len(refused.Add) != 0 {
		t.Fatalf("want singleton refusal, got plan=%+v err=%v", refused, err)
	}
	in.WritePressure = 49
	if idle, err := PlanHotSplit(in); err != nil || len(idle.Add) != 0 {
		t.Fatalf("split despite sufficient relief: plan=%+v err=%v", idle, err)
	}
}

func TestHotTrieDoesNotMutateView(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	before := view.Len()
	digest := view.Digest
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}
	_, _ = PlanHotSplit(HotInput{
		Donor: d, Current: view, WritePressure: 80, TargetWrite: 10,
		Zones: []ZoneStats{{Zone: z, WriteFast: 40}},
	})
	if view.Len() != before || view.Digest != digest {
		t.Fatal("planner mutated V_active")
	}
}

// Advisory subtree heat changes ranking, never the capacity estimate or grant.
func TestHotReliefRanksRecentHeatWithoutClaimingWriteCapacity(t *testing.T) {
	zones := []ZoneStats{
		{Zone: tree.ZoneKey{Collection: "c", Location: "aaa"}, Items: 10000, Bytes: 10000},
		{Zone: tree.ZoneKey{Collection: "c", Location: "bbb"}, Items: 10, Bytes: 10, Heat: 100},
	}
	if got := hottestReliefLeaf(zones, 0, false); got != 0 {
		t.Fatalf("volume choice %d", got)
	}
	if got := hottestReliefLeaf(zones, 0, true); got != 1 {
		t.Fatalf("heat choice %d", got)
	}
	if got := EstimateRelief(zones[1], 100); got.WritesRemoved != 0 || got.MirrorRho != 0 {
		t.Fatalf("advisory heat became capacity: %+v", got)
	}
	if atomicZone(HotInput{AtomicWrite: 1}, zones[1]) {
		t.Fatal("advisory heat declared atomic overload")
	}
}

func TestProcessReliefCanMoveSmallHotAreaAndKeepDonorUseful(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: u, Name: "U"})
	hot := tree.ZoneKey{Collection: "c", Location: "aaa", ID: bitID(94)}
	cold := tree.ZoneKey{Collection: "c", Location: "bbb", ID: bitID(93)}
	zones := []ZoneStats{{Zone: hot, Items: 10, Bytes: 10, Heat: 100}, {Zone: cold, Items: 10000, Bytes: 10000}}
	in := HotInput{Donor: d, Current: view, Zones: zones, PreferWrites: true, ItemPressure: 10010, TargetItems: 10009}
	plan, err := PlanHotSplit(in)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := ValidatePlan(view, plan, zones)
	if err != nil {
		t.Fatal(err)
	}
	if len(checked.Changes) != 1 || checked.Changes[0].Zone != hot {
		t.Fatalf("did not choose small hot area: %+v", checked.Changes)
	}
	in.Zones = zones[:1]
	in.ItemPressure = 10
	in.TargetItems = 9
	if _, err = PlanHotSplit(in); err == nil {
		t.Fatal("single indivisible zone produced an empty donor chain")
	}
}
