package protocol_test

import (
	"testing"

	protocol "github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/testoracle"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestChaos_OwnerCapacityAndNoUnreadiedOwner(t *testing.T) {
	const bits uint8 = 8
	d := tree.ID(testoracle.Packed(1, bits))
	u := tree.ID(testoracle.Packed(200, bits))
	view := tree.NewView(1)
	if err := view.Add("D", d); err != nil {
		t.Fatal(err)
	}
	if err := view.Add("U", u); err != nil {
		t.Fatal(err)
	}
	seed := uint64(0x12345678)
	for step := 0; step < 256; step++ {
		seed = seed*0x5851f42d4c957f2d + 1
		z := tree.ID(testoracle.Packed(seed%(1<<bits), bits))
		active := make([]tree.ID, 0, view.Len())
		for _, desc := range view.Active {
			active = append(active, desc.ID)
		}
		if view.Owner(z) != testoracle.OwnerSlow(z, active) {
			t.Fatalf("step %d owner mismatch", step)
		}
		if step%17 == 0 {
			j := tree.ID(testoracle.Packed(3+uint64(step)%200, bits))
			if j.IsZero() || view.HasID(j) {
				continue
			}
			zones := []protocol.ZoneStats{{
				Zone:      tree.ZoneKey{Collection: "c", Location: "aa", ID: z},
				WriteFast: 40,
			}}
			plan, err := protocol.PlanHotSplit(protocol.HotInput{
				Donor: d, Current: view, WritePressure: 80, TargetWrite: 10, Zones: zones,
			})
			if err != nil {
				continue
			}
			vp, err := protocol.ValidatePlan(view, plan, zones)
			if err != nil {
				t.Fatal(err)
			}
			for _, ch := range vp.Changes {
				if ch.OldOwner != d {
					t.Fatal("unreadied/external owner change")
				}
			}
		}
	}
}
