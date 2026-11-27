package protocol

import (
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestAutoscale_IncrementalPeelUntilPressureRelieved(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	zones := []ZoneStats{
		{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(0)}, WriteFast: 25, ETA: 400 * time.Millisecond},
		{Zone: tree.ZoneKey{Collection: "c", Location: "ab", ID: bitID(1)}, WriteFast: 20, ETA: 400 * time.Millisecond},
		{Zone: tree.ZoneKey{Collection: "c", Location: "ba", ID: bitID(2)}, WriteFast: 10, ETA: 10 * time.Second},
		{Zone: tree.ZoneKey{Collection: "c", Location: "bb", ID: bitID(3)}, WriteFast: 20, ETA: 10 * time.Second},
	}
	st := MachineState{
		MachineID:   "M1",
		Capacity:    10000,
		WriteCap:    100,
		Pressure:    Pressure{Writes: 75},
		TargetWrite: 50,
		Donor:       d,
		View:        view,
		Zones:       zones,
		Offers:      []Offer{{MachineID: "M2", AvailableDisk: 90, AvailableWrite: 90}},
	}
	dec := EvaluateMachine(st)
	if dec.Kind != KindHotSplit {
		t.Fatalf("write-hot vnode must HotTrie, not %v", dec.Kind)
	}
	vp, err := ValidatePlan(view, dec.Plan, zones)
	if err != nil {
		t.Fatal(err)
	}
	moved := map[string]bool{}
	pressure := 75.0
	for _, ch := range vp.Changes {
		moved[ch.Zone.Location] = true
		switch ch.Zone.Location {
		case "aa":
			pressure -= 25
		case "ab":
			pressure -= 20
		case "ba":
			t.Fatal("L3 moved")
		case "bb":
			t.Fatal("cold moved")
		}
	}
	if !moved["aa"] {
		t.Fatal("L1 must peel first")
	}
	cur := vp.Next
	rest := remainingDonorZones(cur, d, zones)
	if pressure >= 50 {
		st.View = cur
		st.Zones = rest
		st.Pressure.Writes = pressure
		dec = EvaluateMachine(st)
		if dec.Kind != KindHotSplit {
			t.Fatalf("pressure %v still >=50, want second HotTrie got %v", pressure, dec.Kind)
		}
		vp, err = ValidatePlan(cur, dec.Plan, rest)
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range vp.Changes {
			if ch.Zone.Location == "ba" || ch.Zone.Location == "bb" {
				t.Fatal("L3/cold moved on second peel")
			}
			if ch.Zone.Location == "ab" {
				pressure -= 20
			}
		}
		cur = vp.Next
	}
	if pressure >= 50 {
		t.Fatalf("pressure stuck at %v", pressure)
	}
	if cur.Owner(bitID(2)) != d || cur.Owner(bitID(3)) != d {
		t.Fatal("L3 and cold must stay on D")
	}
}

func remainingDonorZones(view *tree.View, donor tree.ID, zones []ZoneStats) []ZoneStats {
	var out []ZoneStats
	for _, z := range zones {
		if view.Owner(z.ID()) == donor {
			out = append(out, z)
		}
	}
	return out
}

func TestAutoscale_DiskPressureMovesNotHotTrie(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	dec := EvaluateMachine(MachineState{
		MachineID:  "M1",
		Capacity:   100,
		WriteCap:   100,
		Usage:      Usage{Owned: 20},
		Pressure:   Pressure{Disk: 95},
		TargetDisk: 90,
		Donor:      d,
		View:       view,
		Offers:     []Offer{{MachineID: "M2", AvailableDisk: 90, AvailableWrite: 90}},
		Zones: []ZoneStats{
			{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(90)}, Items: 20, WriteFast: 1},
			{Zone: tree.ZoneKey{Collection: "c", Location: "ab", ID: bitID(91)}, Items: 18, WriteFast: 1},
			{Zone: tree.ZoneKey{Collection: "c", Location: "ba", ID: bitID(92)}, Items: 17, WriteFast: 1},
		},
	})
	if dec.Kind != KindMove {
		t.Fatalf("machine-full with light vnodes must MOVE, got %v", dec.Kind)
	}
	if len(dec.Plan.Add) != 0 || dec.Plan.Reason != ReasonMoveHost {
		t.Fatal("HotTrie/Regional must not run for disk-only MOVE")
	}
}

func TestAutoscale_MoveBeforeSplit(t *testing.T) {
	TestAUTO1_MoveBeforeHotWhenDiskOnly(t)
}

func TestAutoscale_HotTrieAfterMoveInsufficient(t *testing.T) {
	TestAUTO1_WritePrefersHotTrieNotMove(t)
}

func TestAutoscale_RegionalAfterSafeSplitFailure(t *testing.T) {
	d := bitID(95)
	u := tree.SetBit(d, 10, 1)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"}, tree.VNodeDescriptor{ID: u, Name: "U"})
	z := tree.ZoneKey{Collection: "c", Location: "aa", ID: tree.SetBit(d, 5, 1)}
	dec := EvaluateMachine(MachineState{
		MachineID:   "M1",
		Capacity:    10000,
		WriteCap:    1000,
		Pressure:    Pressure{Writes: 80},
		TargetWrite: 10,
		Donor:       d,
		View:        view,
		Zones:       []ZoneStats{{Zone: z, WriteFast: 80}},
	})
	if dec.Kind != KindRegionalAdd {
		t.Fatalf("kind=%v want REGIONAL", dec.Kind)
	}
}

func TestAutoscale_AtomicStops(t *testing.T) {
	TestEvaluateAtomicDoesNotLoop(t)
}

func TestAutoscale_DiskTriggerInclusive(t *testing.T) {
	m := MachineState{Capacity: 100, TargetDisk: 90, Pressure: Pressure{Disk: 89}}
	disk, _ := underPressure(m)
	if disk {
		t.Fatal("89 must not trigger at threshold 90")
	}
	m.Pressure.Disk = 90
	disk, _ = underPressure(m)
	if !disk {
		t.Fatal("90 >= 90 must trigger")
	}
	m.Pressure.Disk = 91
	disk, _ = underPressure(m)
	if !disk {
		t.Fatal("91 must trigger")
	}
}

func TestAutoscale_WriteTriggerInclusive(t *testing.T) {
	m := MachineState{WriteCap: 100, TargetWrite: 70, Pressure: Pressure{Writes: 69}}
	_, write := underPressure(m)
	if write {
		t.Fatal("69 must not trigger at 70")
	}
	m.Pressure.Writes = 70
	_, write = underPressure(m)
	if !write {
		t.Fatal("70 >= 70 must trigger")
	}
}

func TestAUTO2_PositiveGrowthScalesNow(t *testing.T) {
	m := MachineState{
		Capacity:   100,
		Pressure:   Pressure{Disk: 80, DiskGrowth: 5},
		ScaleTime:  6 * time.Second,
		TargetDisk: 95,
	}
	disk, _ := underPressure(m)
	if !disk {
		t.Fatal("T_exhaust=4s < 2*6s must scale now")
	}
}

func TestAUTO2_FarExhaustionWaits(t *testing.T) {
	m := MachineState{
		Capacity:   100,
		Pressure:   Pressure{Disk: 60, DiskGrowth: 1},
		ScaleTime:  5 * time.Second,
		TargetDisk: 95,
	}
	disk, _ := underPressure(m)
	if disk {
		t.Fatal("T_exhaust=40s should not scale early")
	}
}

func TestAUTO2_ZeroGrowthDoesNotScale(t *testing.T) {
	m := MachineState{Capacity: 100, Pressure: Pressure{Disk: 80, DiskGrowth: 0}, TargetDisk: 95}
	disk, _ := underPressure(m)
	if disk {
		t.Fatal("zero growth uses threshold only")
	}
}

func TestAUTO2_NegativeGrowthDoesNotScale(t *testing.T) {
	m := MachineState{Capacity: 100, Pressure: Pressure{Disk: 80, DiskGrowth: -1}, TargetDisk: 95}
	disk, _ := underPressure(m)
	if disk {
		t.Fatal("negative growth must not trigger exhaustion")
	}
}

func TestAUTO2_UnknownGrowthUsesThreshold(t *testing.T) {
	TestAUTO2_ZeroGrowthDoesNotScale(t)
}

func TestFuzzHostMove_DoesNotChangeOwnership(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	digest := view.Digest
	gen := view.Generation
	dec := EvaluateMachine(MachineState{
		MachineID:  "A",
		Capacity:   100,
		Usage:      Usage{Owned: 80},
		Pressure:   Pressure{Disk: 80},
		TargetDisk: 50,
		Donor:      d,
		View:       view,
		Offers:     []Offer{{MachineID: "B", AvailableDisk: 90, AvailableWrite: 90}},
	})
	if dec.Kind != KindMove {
		t.Fatalf("kind=%v", dec.Kind)
	}
	vp, err := ValidatePlan(view, dec.Plan, []ZoneStats{{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(vp.Changes) != 0 {
		t.Fatal("MOVE must not change owners")
	}
	if view.Digest != digest || view.Generation != gen {
		t.Fatal("logical topology mutated by MOVE decision")
	}
}

func TestAutoscale_SpikeDoesNotPoisonEWMAForever(t *testing.T) {
	e := RateEWMA{Alpha: 0.2}
	e.Observe(10)
	e.Observe(1000)
	for i := 0; i < 30; i++ {
		e.Observe(10)
	}
	if e.Value > 50 {
		t.Fatalf("EWMA still poisoned by the spike: %v", e.Value)
	}
}
