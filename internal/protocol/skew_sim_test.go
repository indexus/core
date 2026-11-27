package protocol

import (
	"math/rand/v2"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// TestDeterministicSkewAcross24UniformSplits simulates 4 -> 24 vnodes under
// uniform keys using the real EvaluateMachine + tree.View XOR geometry.
func TestDeterministicSkewAcross24UniformSplits(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 99))

	// Generate 4000 uniform zones across 16 collections with random 16-char locations (matching cmd/experiment).
	cols := make([]string, 16)
	for i := range cols {
		var raw [tree.Size]byte
		for b := range raw {
			raw[b] = byte(rng.Uint32())
		}
		cols[i] = tree.EncodeName(raw)
	}
	zones := make([]ZoneStats, 4000)
	for i := range zones {
		var raw [tree.Size]byte
		for b := range raw {
			raw[b] = byte(rng.Uint32())
		}
		loc := tree.EncodeName(raw)
		zk, err := tree.NewZoneKey(cols[i%len(cols)], loc)
		if err != nil {
			t.Fatal(err)
		}
		zones[i] = ZoneStats{Zone: zk, Items: 10, Bytes: 1024}
	}

	// Start with 4 evenly-spaced initial vnodes (matching cluster boot).
	var initial []tree.VNodeDescriptor
	for i := range 4 {
		var id tree.ID
		id[0] = byte(i << 6)
		id[1] = byte(i + 1)
		initial = append(initial, tree.VNodeDescriptor{ID: id, Name: tree.EncodeName(id)})
	}
	for _, tc := range []struct {
		name      string
		rebalance bool
	}{
		{name: "items_limit", rebalance: false},
		{name: "writes_rebalance", rebalance: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runSkewSim(t, rng, zones, initial, tc.rebalance)
		})
	}
}

func runSkewSim(t *testing.T, rng *rand.Rand, zones []ZoneStats, initial []tree.VNodeDescriptor, rebalance bool) {
	t.Helper()
	view := tree.NewView(1)
	for _, d := range initial {
		var err error
		view, err = tree.Extend(view, d)
		if err != nil {
			t.Fatal(err)
		}
	}

	countByOwner := func(v *tree.View) map[tree.ID]int64 {
		m := make(map[tree.ID]int64, v.Len())
		for _, d := range v.Active {
			m[d.ID] = 0
		}
		for _, z := range zones {
			m[v.Owner(z.ID())] += z.Items
		}
		return m
	}

	hotSplits, regionalAdds := 0, 0
	for view.Len() < 24 {
		counts := countByOwner(view)
		var donor tree.VNodeDescriptor
		var maxItems int64
		for _, d := range view.Active {
			if counts[d.ID] > maxItems {
				maxItems = counts[d.ID]
				donor = d
			}
		}
		st := MachineState{
			MachineID: MachineID(donor.Name),
			Capacity:  1 << 30,
			View:      view,
			Donor:     donor.ID,
			Zones:     zones,
			Rebalance: rebalance,
		}
		if !rebalance {
			st.ItemLimit = maxItems
		}
		dec := EvaluateMachine(st)
		switch dec.Kind {
		case KindHotSplit:
			hotSplits++
		case KindRegionalAdd:
			regionalAdds++
		default:
			t.Fatalf("at %d vnodes: EvaluateMachine returned %v (err=%v)", view.Len(), dec.Kind, dec.Err)
		}
		target := dec.Plan.Add[0].ID
		for b := 8; b < tree.Size; b++ {
			target[b] = byte(rng.Uint32())
		}
		next, err := tree.Extend(view, tree.VNodeDescriptor{ID: target, Name: tree.EncodeName(target)})
		if err != nil {
			t.Fatal(err)
		}
		view = next
	}

	counts := countByOwner(view)
	var maxC, minC, sumC int64
	minC = 1 << 60
	for _, c := range counts {
		sumC += c
		if c > maxC {
			maxC = c
		}
		if c < minC {
			minC = c
		}
	}
	mean := float64(sumC) / float64(view.Len())
	skew := float64(maxC) / mean
	t.Logf("24 vnodes (%v): hotSplits=%d regionalAdds=%d min=%d mean=%.1f max=%d skew=%.2f",
		rebalance, hotSplits, regionalAdds, minC, mean, maxC, skew)
	if skew > 2.2 {
		t.Fatalf("excessive XOR skew=%.2f on uniform keys (hotSplits=%d regionalAdds=%d)", skew, hotSplits, regionalAdds)
	}
}
