package protocol

import (
	"errors"
	"testing"

	"github.com/indexus/go-indexus-core/internal/testoracle"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestREG1_EveryKeyOutsideRegionUnchanged(t *testing.T) {
	const bits uint8 = 8
	d := tree.ID(testoracle.Packed(0x80, bits))
	u := tree.ID(testoracle.Packed(0x01, bits))
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: u, Name: "U"})
	digest := view.Digest
	var zones []ZoneStats
	var keys []tree.ID
	for k := uint64(0); k < 1<<bits; k++ {
		id := tree.ID(testoracle.Packed(k, bits))
		keys = append(keys, id)
		zones = append(zones, ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "z", ID: id}})
	}
	legal := 0
	for b := uint8(1); b < bits; b++ {
		region := tree.Prefix{ID: d, Bits: b}
		for k := uint64(0); k < 1<<bits; k++ {
			j := tree.ID(testoracle.Packed(k, bits))
			if j == d || j == u || j.IsZero() || !region.Contains(j) {
				continue
			}
			plan := TopologyPlan{
				BaseDigest: view.Digest,
				Donor:      d,
				Reason:     ReasonRegionalAdd,
				Region:     region,
				Add:        []tree.VNodeDescriptor{{ID: j, Name: "J"}},
			}
			vp, err := ValidatePlan(view, plan, zones)
			if view.Digest != digest || view.Len() != 2 {
				t.Fatal("validation mutated the live view")
			}
			if err != nil {
				if !errors.Is(err, ErrExternalSteal) {
					t.Fatal(err)
				}
				continue
			}
			legal++
			for _, z := range keys {
				if region.Contains(z) {
					continue
				}
				if vp.Next.Owner(z) != view.Owner(z) {
					t.Fatalf("key outside region changed owner at bits=%d", b)
				}
			}
		}
	}
	if legal == 0 {
		t.Fatal("no regional add stayed inside its region")
	}
}
