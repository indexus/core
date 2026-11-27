package protocol

import (
	"errors"
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// TestProtocolContract is the short form of the protocol. Heavier fuzz tests
// prove the same rules on more IDs; they do not add rules.
func TestProtocolContract(t *testing.T) {
	t.Run("OWN-1 owner is pure XOR", TestOWN1_OwnerIsArgminXOR)
	t.Run("PLAN-1 validated plans are complete", TestPLAN1_IgnorePlannerPredictions)
	t.Run("HOT-1 donor-only ADD cannot steal", TestHOT1_NoExternalSteal)
	t.Run("REG-1 regional ADD is confined", TestREG1_NoChangeOutsideRegion)
	t.Run("HND-3 single write leader", TestHND3_SingleWriteLeader)
}

func TestOWN1_OwnerIsArgminXOR(t *testing.T) {
	d, j := bitID(95), bitID(0)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: j, Name: "J"})
	z := bitID(94)
	if view.Owner(z) != testoracle.OwnerSlow(z, []tree.ID{d, j}) {
		t.Fatal("View.Owner diverged from argmin XOR")
	}
	again := view.Owner(z)
	if again != view.Owner(z) {
		t.Fatal("owner is not deterministic")
	}
}

func TestPLAN1_IgnorePlannerPredictions(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: u, Name: "U"})
	j := bitID(94)
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: j}
	plan := TopologyPlan{
		BaseDigest: view.Digest,
		Add:        []tree.VNodeDescriptor{{ID: j, Name: "J"}},
		Changes:    []ZoneChange{{Zone: z, OldOwner: u, NewOwner: u}},
	}
	got, err := ValidatePlan(view, plan, []ZoneStats{{Zone: z}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) == 0 {
		t.Fatal("XOR ownership change was hidden")
	}
	for _, ch := range got.Changes {
		if ch.OldOwner == u && ch.NewOwner == u {
			t.Fatal("predicted owners leaked through ValidatePlan")
		}
	}
}

func TestHOT1_NoExternalSteal(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: u, Name: "U"})
	j := tree.SetBit(u, 1, 1)
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: j}
	plan := TopologyPlan{
		BaseDigest: view.Digest,
		Donor:      d,
		Reason:     ReasonHotSplit,
		Add:        []tree.VNodeDescriptor{{ID: j, Name: "J"}},
	}
	_, err := ValidatePlan(view, plan, []ZoneStats{{Zone: z}})
	if !errors.Is(err, ErrExternalSteal) {
		t.Fatalf("want steal outside donor, got %v", err)
	}
}

func TestREG1_NoChangeOutsideRegion(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: u, Name: "U"})
	j := tree.SetBit(u, 1, 1)
	plan := TopologyPlan{
		BaseDigest: view.Digest,
		Donor:      d,
		Reason:     ReasonRegionalAdd,
		Region:     tree.Prefix{ID: d, Bits: 96},
		Add:        []tree.VNodeDescriptor{{ID: j, Name: "J"}},
	}
	_, err := ValidatePlan(view, plan, nil)
	if !errors.Is(err, ErrExternalSteal) {
		t.Fatalf("want vnode outside region, got %v", err)
	}
}

func TestHND3_SingleWriteLeader(t *testing.T) {
	d, j := bitID(95), bitID(0)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: j, Name: "J"})
	z := bitID(94)
	leaders := 0
	for _, me := range []tree.ID{d, j} {
		if view.Owner(z) == me {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("write leaders=%d", leaders)
	}
}
