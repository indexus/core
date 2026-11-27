package protocol

import (
	"errors"
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func viewIDs(v *tree.View) []tree.ID {
	out := make([]tree.ID, 0, v.Len())
	for _, d := range v.Active {
		out = append(out, d.ID)
	}
	return out
}

func TestValidatePlan_IgnoresPredictedOwners(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: u, Name: "U"})
	z1 := tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}
	j, err := tree.HotSplitAddress(d, Guard(view, d))
	if err != nil {
		t.Fatal(err)
	}
	plan := TopologyPlan{
		BaseDigest: view.Digest,
		Donor:      d,
		Add:        []tree.VNodeDescriptor{{ID: j, Name: "J"}},
		Reason:     ReasonHotSplit,
		Changes:    []ZoneChange{{Zone: z1, OldOwner: u, NewOwner: u}},
	}
	got, err := ValidatePlan(view, plan, []ZoneStats{{Zone: z1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range got.Changes {
		if ch.OldOwner == u && ch.NewOwner == u {
			t.Fatal("predicted owners leaked through")
		}
	}
}

func TestValidatePlan_ComputesEveryChangedZone(t *testing.T) {
	d, x := bitID(95), bitID(0)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: x, Name: "X"},
	)
	j := bitID(94)
	next, err := tree.Extend(view, tree.VNodeDescriptor{ID: j, Name: "J"})
	if err != nil {
		t.Fatal(err)
	}
	z1 := tree.ZoneKey{Collection: "c", Location: "a1", ID: j}
	z2 := tree.ZoneKey{Collection: "c", Location: "a2", ID: tree.SetBit(j, 93, 1)}
	z3 := tree.ZoneKey{Collection: "c", Location: "a3", ID: tree.SetBit(j, 92, 1)}
	zones := []ZoneStats{{Zone: z1}, {Zone: z2}, {Zone: z3}}
	plan := TopologyPlan{
		BaseDigest: view.Digest,
		Add:        []tree.VNodeDescriptor{{ID: j, Name: "J"}},
		Changes:    []ZoneChange{{Zone: z1, OldOwner: d, NewOwner: j}},
	}
	got, err := ValidatePlan(view, plan, zones)
	if err != nil {
		t.Fatal(err)
	}
	want := testoracle.OwnershipDiff(viewIDs(view), viewIDs(next), []tree.ID{z1.ID, z2.ID, z3.ID})
	if len(got.Changes) != len(want) {
		t.Fatalf("got %d changes want %d (planner hid XOR consequences)", len(got.Changes), len(want))
	}
}

func TestValidatePlan_NoPhantomChange(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}
	plan := TopologyPlan{BaseDigest: view.Digest}
	got, err := ValidatePlan(view, plan, []ZoneStats{{Zone: z}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != 0 {
		t.Fatalf("phantom changes %d", len(got.Changes))
	}
}

func TestValidatePlan_AddRemoveTogether(t *testing.T) {
	d, u := bitID(95), bitID(0)
	j := bitID(94)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	plan := TopologyPlan{
		BaseDigest: view.Digest,
		Add:        []tree.VNodeDescriptor{{ID: j, Name: "J"}},
		Remove:     []string{"U"},
	}
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: u}
	got, err := ValidatePlan(view, plan, []ZoneStats{{Zone: z}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Next.HasName("U") || !got.Next.HasName("J") {
		t.Fatal("add+remove not applied")
	}
}

func TestValidatePlan_RejectsDuplicateVNodeID(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	_, err := ValidatePlan(view, TopologyPlan{
		BaseDigest: view.Digest,
		Add:        []tree.VNodeDescriptor{{ID: d, Name: "J"}},
	}, nil)
	if err == nil {
		t.Fatal("duplicate id must fail")
	}
}

func TestValidatePlan_RejectsStaleBaseDigest(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	_, err := ValidatePlan(view, TopologyPlan{BaseDigest: [32]byte{1, 2, 3}}, nil)
	if !errors.Is(err, ErrStalePlan) {
		t.Fatalf("got %v", err)
	}
}

func TestValidatePlan_Conservation(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	j := bitID(94)
	zones := make([]ZoneStats, 0, 32)
	ids := make([]tree.ID, 0, 32)
	for i := uint64(0); i < 32; i++ {
		id := tree.ID(testoracle.Packed(i, 8))
		zk := tree.ZoneKey{Collection: "c", Location: string(rune('a' + i%26)), ID: id}
		zones = append(zones, ZoneStats{Zone: zk})
		ids = append(ids, id)
	}
	plan := TopologyPlan{BaseDigest: view.Digest, Add: []tree.VNodeDescriptor{{ID: j, Name: "J"}}}
	got, err := ValidatePlan(view, plan, zones)
	if err != nil {
		t.Fatal(err)
	}
	want := testoracle.OwnershipDiff(viewIDs(view), viewIDs(got.Next), ids)
	if len(want) != len(got.Changes) {
		t.Fatalf("conservation: oracle %d validate %d", len(want), len(got.Changes))
	}
}

func TestValidatePlanRejectsExternalStealWithoutExistingRows(t *testing.T) {
	for _, reason := range []PlanReason{ReasonHotSplit, ReasonRegionalAdd} {
		t.Run(reason.String(), func(t *testing.T) {
			// Empty inventory does not mean empty ownership. J=129 takes keys from U
			// outside D's cell; in the regional case J lies inside R but its cell does not.
			current := mustView(t, tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, tree.VNodeDescriptor{Name: "U", ID: tree.ID{128}})
			plan := TopologyPlan{BaseDigest: current.Digest, Reason: reason, Donor: tree.ID{1}, Add: []tree.VNodeDescriptor{{Name: "J", ID: tree.ID{129}}}}
			if reason == ReasonRegionalAdd {
				plan.Region = tree.Prefix{ID: tree.ID{129}, Bits: 8}
			}
			if _, err := ValidatePlan(current, plan, nil); !errors.Is(err, ErrExternalSteal) {
				t.Fatalf("unobserved keys escaped declared scope: %v", err)
			}
		})
	}
}

func TestValidatePlanRequiresCanonicalSingleBitHotSplit(t *testing.T) {
	d, u := tree.ID{1}, tree.ID{128}
	current := mustView(t, tree.VNodeDescriptor{Name: "D", ID: d}, tree.VNodeDescriptor{Name: "U", ID: u})
	guard := Guard(current, d)
	bit := guard + 1
	canonical, err := tree.HotSplitAddress(d, bit)
	if err != nil {
		t.Fatal(err)
	}
	plan := TopologyPlan{
		BaseDigest: current.Digest,
		Donor:      d,
		Add:        []tree.VNodeDescriptor{{Name: tree.EncodeName(canonical), ID: canonical}},
		Reason:     ReasonHotSplit,
	}
	if _, err := ValidatePlan(current, plan, nil); err != nil {
		t.Fatalf("canonical split refused: %v", err)
	}
	nonCanonical := tree.SetBit(canonical, bit+1, 1-tree.Bit(canonical, bit+1))
	plan.Add[0] = tree.VNodeDescriptor{Name: tree.EncodeName(nonCanonical), ID: nonCanonical}
	if _, err := ValidatePlan(current, plan, nil); !errors.Is(err, ErrExternalSteal) {
		t.Fatalf("multi-bit hot split accepted: %v", err)
	}
}
