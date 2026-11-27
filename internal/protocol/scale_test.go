package protocol

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestAUTO1_MoveBeforeHotWhenDiskOnly(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	dec := EvaluateMachine(MachineState{
		MachineID:  "M1",
		Capacity:   100,
		WriteCap:   100,
		Usage:      Usage{Owned: 80},
		Pressure:   Pressure{Disk: 80},
		TargetDisk: 50,
		Donor:      d,
		View:       view,
		Offers: []Offer{
			{MachineID: "M2", AvailableDisk: 90, AvailableWrite: 90},
		},
		Zones: []ZoneStats{{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}, Items: 80, WriteFast: 1}},
	})
	if dec.Kind != KindMove || dec.Move == nil || dec.Move.To != "M2" {
		t.Fatalf("kind=%v move=%+v", dec.Kind, dec.Move)
	}
}

func TestAUTO1_WritePrefersHotTrieNotMove(t *testing.T) {
	d, u := bitID(95), bitID(0)
	view := mustView(t,
		tree.VNodeDescriptor{ID: d, Name: "D"},
		tree.VNodeDescriptor{ID: u, Name: "U"},
	)
	dec := EvaluateMachine(MachineState{
		MachineID:   "M1",
		Capacity:    1000,
		WriteCap:    100,
		Pressure:    Pressure{Disk: 10, Writes: 80},
		TargetDisk:  900,
		TargetWrite: 50,
		Donor:       d,
		View:        view,
		Offers: []Offer{
			{MachineID: "M2", AvailableDisk: 900, AvailableWrite: 90},
		},
		Zones: []ZoneStats{{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}, Items: 10, WriteFast: 40}},
	})
	if dec.Kind != KindHotSplit {
		t.Fatalf("kind=%v want HOT_SPLIT", dec.Kind)
	}
	if _, err := ValidatePlan(view, dec.Plan, []ZoneStats{{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}, WriteFast: 40}}); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluateAtomicDoesNotLoop(t *testing.T) {
	d := bitID(95)
	view := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	dec := EvaluateMachine(MachineState{
		MachineID:   "M1",
		Capacity:    100,
		WriteCap:    50,
		Pressure:    Pressure{Writes: 80},
		TargetWrite: 10,
		Donor:       d,
		View:        view,
		Zones:       []ZoneStats{{Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: bitID(94)}, WriteFast: 80}},
	})
	if dec.Kind != KindAtomic {
		t.Fatalf("kind=%v want ATOMIC", dec.Kind)
	}
}

func TestEvaluateNoneWhenCold(t *testing.T) {
	dec := EvaluateMachine(MachineState{Pressure: Pressure{Disk: 1, Writes: 1}, TargetDisk: 50, TargetWrite: 50})
	if dec.Kind != KindNone {
		t.Fatalf("kind=%v", dec.Kind)
	}
}
