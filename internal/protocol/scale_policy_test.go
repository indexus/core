package protocol

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestScalePolicyItemLoadIsDistinctFromByteCapacity(t *testing.T) {
	d := bitID(95)
	v := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	zones := []ZoneStats{{Zone: tree.ZoneKey{Collection: "c", Location: "a", ID: bitID(90)}, Items: 40, Bytes: 400}, {Zone: tree.ZoneKey{Collection: "c", Location: "b", ID: bitID(91)}, Items: 40, Bytes: 400}}
	st := MachineState{Donor: d, View: v, Capacity: 1 << 20, Pressure: Pressure{Disk: 1000}, ItemLimit: 60, Zones: zones}
	dec := EvaluateMachine(st)
	if dec.Kind != KindHotSplit && dec.Kind != KindRegionalAdd {
		t.Fatalf("kind=%d error=%v", dec.Kind, dec.Err)
	}
	if _, err := ValidatePlan(v, dec.Plan, zones); err != nil {
		t.Fatal(err)
	}
	next, err := tree.Extend(v, dec.Plan.Add...)
	if err != nil {
		t.Fatal(err)
	}
	var retained int64
	for _, z := range zones {
		if next.Owner(z.ID()) == d {
			retained += z.Items
		}
	}
	if retained <= 0 || retained >= 80 {
		t.Fatalf("not a useful split: retained=%d", retained)
	}
	if st.Pressure.Disk != 1000 || st.Capacity != 1<<20 {
		t.Fatal("planner modified physical accounting")
	}
}

func TestScalePolicyCoincidentZonesCannotManufactureRelief(t *testing.T) {
	d := bitID(95)
	v := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	same := bitID(90)
	zones := []ZoneStats{{Zone: tree.ZoneKey{Collection: "c", Location: "a", ID: same}, Items: 4, Bytes: 400}, {Zone: tree.ZoneKey{Collection: "c", Location: "aa", ID: same}, Items: 4, Bytes: 400}}
	dec := EvaluateMachine(MachineState{Donor: d, View: v, Capacity: 1 << 20, ItemLimit: 4, Zones: zones})
	if len(dec.Plan.Add) != 0 {
		t.Fatalf("coincident addresses cannot divide their load: %+v", dec)
	}
	if v.Len() != 1 {
		t.Fatal("rejected plan changed view")
	}
}

func TestScalePolicyDiskReliefUsesBytesWhenPresent(t *testing.T) {
	d := bitID(95)
	v := mustView(t, tree.VNodeDescriptor{ID: d, Name: "D"})
	z := ZoneStats{Zone: tree.ZoneKey{Collection: "c", Location: "a", ID: bitID(90)}, Items: 1, Bytes: 700}
	plan, err := PlanHotSplit(HotInput{Donor: d, Current: v, Zones: []ZoneStats{z}, DiskPressure: 900, TargetDisk: 500, AtomicDisk: 1000})
	if err != nil || len(plan.Add) == 0 {
		t.Fatalf("700 bytes should relieve 400 byte deficit: %v %+v", err, plan)
	}
	if _, err := ValidatePlan(v, plan, []ZoneStats{z}); err != nil {
		t.Fatal(err)
	}
	_, err = PlanHotSplit(HotInput{Donor: d, Current: v, Zones: []ZoneStats{z}, DiskPressure: 900, TargetDisk: 500, AtomicDisk: 500})
	if err != ErrAtomicOverload {
		t.Fatalf("byte atomic limit ignored: %v", err)
	}
}
