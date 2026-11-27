package protocol

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestHotGrowthSkipsRetiredAddresses(t *testing.T) {
	donor, other := bitID(95), bitID(0)
	view := mustView(t, tree.VNodeDescriptor{ID: donor, Name: "donor"}, tree.VNodeDescriptor{ID: other, Name: "other"})
	hotID := tree.SetBit(bitID(94), 93, 1)
	hot := tree.ZoneKey{Collection: "c", Location: "hot", ID: hotID}
	original, ok := forkForZone(donor, Guard(view, donor), ZoneStats{Zone: hot})
	if !ok {
		t.Fatal("fixture has no fork")
	}
	view.ReserveRetired(original.ID)
	if err := view.Add("resurrect", original.ID); err == nil {
		t.Fatal("retired address reused")
	}
	if !view.Clone().IsRetiredID(original.ID) {
		t.Fatal("clone lost namespace exclusion")
	}
	plan, err := PlanHotSplit(HotInput{Donor: donor, Current: view, WritePressure: 80, TargetWrite: 50, Zones: []ZoneStats{{Zone: hot, Items: 100, WriteFast: 40}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) == 0 {
		t.Fatal("growth stopped despite another safe bit")
	}
	for _, added := range plan.Add {
		if added.ID == original.ID {
			t.Fatal("hot split resurrected retired address")
		}
	}
	checked, err := ValidatePlan(view, plan, []ZoneStats{{Zone: hot}})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range checked.Changes {
		if change.OldOwner != donor {
			t.Fatal("growth stole another vnode's zone")
		}
	}
}
