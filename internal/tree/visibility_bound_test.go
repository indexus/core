package tree_test

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// A passing local Guard check is conditional on the supplied membership.
// This counterexample must remain visible: it does not authorize using a
// global oracle in production, nor does geometric movement grant disk writes.
func TestHotSplitGuardCannotCertifyAnOmittedCompetitor(t *testing.T) {
	donor, hidden := tree.ID{1}, tree.ID{128}
	localGuard := tree.GuardBit(donor, []tree.ID{donor})
	joiner, err := tree.HotSplitAddress(donor, localGuard)
	if err != nil {
		t.Fatal(err)
	}
	realGuard := testoracle.GuardSlow(donor, []tree.ID{donor, hidden})
	before := testoracle.OwnerSlow(joiner, []tree.ID{donor, hidden})
	after := testoracle.OwnerSlow(joiner, []tree.ID{donor, hidden, joiner})
	if localGuard >= realGuard || before != hidden || after != joiner {
		t.Fatal("fixture no longer demonstrates the incomplete-view hazard")
	}
	t.Logf("local guard=%d actual guard=%d; key %s changes from hidden peer to joiner", localGuard, realGuard, joiner)
}

func TestCompleteViewHotWavesCommuteWithoutExternalCapture(t *testing.T) {
	active := []tree.ID{{1}, {200}}
	changes := 0
	for wave := 0; wave < 4; wave++ {
		parent := map[tree.ID]tree.ID{}
		var adds []tree.ID
		// Each donor plans from the same complete pre-wave view, without seeing
		// the other donors' new children. Their exclusive captures must commute.
		for _, donor := range active {
			guard := tree.GuardBit(donor, active)
			if guard >= 8 {
				continue
			}
			child, err := tree.HotSplitAddress(donor, guard)
			if err != nil {
				continue
			}
			if _, duplicate := parent[child]; duplicate {
				t.Fatal("two donors generated the same fork")
			}
			parent[child] = donor
			adds = append(adds, child)
		}
		if len(adds) == 0 {
			t.Fatal("wave exercised no addition")
		}
		forward := append(append([]tree.ID(nil), active...), adds...)
		reverse := append([]tree.ID(nil), active...)
		for i := len(adds) - 1; i >= 0; i-- {
			reverse = append(reverse, adds[i])
		}
		for key := 0; key < 256; key++ {
			point := tree.ID{byte(key)}
			old := testoracle.OwnerSlow(point, active)
			owner := testoracle.OwnerSlow(point, forward)
			if owner != testoracle.OwnerSlow(point, reverse) {
				t.Fatal("addition order changed ownership")
			}
			if owner != old {
				changes++
				if parent[owner] != old {
					t.Fatalf("wave %d key %02x moved outside its planned donor", wave, key)
				}
			}
		}
		active = forward
	}
	t.Logf("4 waves, %d final vnodes, 1024 exhaustive key checks, %d permitted donor changes; forward/reverse arrival orders agree", len(active), changes)
}
