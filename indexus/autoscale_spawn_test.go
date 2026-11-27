package indexus

import (
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestSpawnPlan_PreserveValidatedDelta(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	n.machine.WriteCap = 100
	for i := 0; i < 80; i++ {
		n.noteZoneWrite("Demo000000000000", "aa")
	}
	dec := n.evaluateScaleDecision()
	if dec.Kind != KindHotSplit && dec.Kind != KindRegionalAdd {
		t.Fatalf("kind=%v", dec.Kind)
	}
	vp, err := ValidatePlan(n.topologyView(), dec.Plan, mustZoneStats(t, n))
	if err != nil {
		t.Fatal(err)
	}
	tags := n.namesFromScalePlan(dec, 1)
	if len(tags) != len(vp.Plan.Add) {
		t.Fatalf("tags=%d adds=%d", len(tags), len(vp.Plan.Add))
	}
	var add []VNodeDescriptor
	for _, tag := range tags {
		id, err := tree.DecodeName(tag)
		if err != nil {
			t.Fatal(err)
		}
		add = append(add, VNodeDescriptor{ID: id, Name: tag})
	}
	rebuilt, err := tree.Apply(vp.Current, tree.Delta{BaseDigest: vp.Current.Digest, Add: add})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Digest != vp.Next.Digest {
		t.Fatal("spawn tags rebuilt a different topology digest")
	}
}

func TestSpawnPlan_PreserveForkCertificate(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	n.machine.WriteCap = 100
	for i := 0; i < 80; i++ {
		n.noteZoneWrite("Demo000000000000", "aa")
	}
	dec := n.evaluateScaleDecision()
	if dec.Kind != KindHotSplit && dec.Kind != KindRegionalAdd {
		t.Fatalf("kind=%v", dec.Kind)
	}
	vp, err := ValidatePlan(n.topologyView(), dec.Plan, mustZoneStats(t, n))
	if err != nil {
		t.Fatal(err)
	}
	g := Guard(vp.Current, vp.Plan.Donor)
	for _, d := range vp.Plan.Add {
		lcp := tree.CommonPrefixLen(vp.Plan.Donor, d.ID)
		if vp.Plan.Reason == ReasonHotSplit && lcp < g {
			t.Fatalf("hot fork LCP=%d < g(D)=%d", lcp, g)
		}
	}
	tags := n.namesFromScalePlan(dec, 1)
	seen := map[string]bool{}
	for _, tag := range tags {
		seen[tag] = true
	}
	for _, d := range vp.Plan.Add {
		if !seen[d.Name] {
			t.Fatalf("fork %s dropped by spawn tags", d.Name)
		}
	}
}

func FuzzSpawnPlan_RoundTrip(f *testing.F) {
	f.Add([]byte{1, 0, 0}, []byte{2, 0, 0}, []byte{3, 0, 0})
	f.Fuzz(func(t *testing.T, db, ub, zb []byte) {
		d, u, z := FromSlice(db), FromSlice(ub), FromSlice(zb)
		if d.IsZero() || u.IsZero() || d == u {
			return
		}
		fixture := newReadPhaseFixture(t, []VNodeDescriptor{{ID: d, Name: EncodeName(d)}, {ID: u, Name: EncodeName(u)}}, false, false)
		n := fixture.nodes[0]
		view := n.topologyView()
		zones := []ZoneStats{{
			Zone:      tree.ZoneKey{Collection: "c", Location: "aa", ID: z},
			WriteFast: 40,
		}}
		plan, err := PlanHotSplit(HotInput{
			Donor: d, Current: view, WritePressure: 80, TargetWrite: 10, Zones: zones,
		})
		if err != nil || len(plan.Add) == 0 {
			return
		}
		vp, err := ValidatePlan(view, plan, zones)
		if err != nil {
			t.Fatal(err)
		}
		tags := n.namesFromScaleState(Decision{Kind: KindHotSplit, Plan: plan}, 1, MachineState{View: view, Zones: zones})
		var add []VNodeDescriptor
		got := map[ID]bool{}
		for _, tag := range tags {
			id, err := tree.DecodeName(tag)
			if err != nil {
				t.Fatal(err)
			}
			add = append(add, VNodeDescriptor{ID: id, Name: tag})
			got[id] = true
		}
		for _, desc := range vp.Plan.Add {
			if !got[desc.ID] {
				t.Fatal("spawn tags missing a validated vnode")
			}
		}
		if len(got) > len(vp.Plan.Add) {
			t.Fatal("spawn tags invented an extra vnode")
		}
		rebuilt, err := tree.Apply(vp.Current, tree.Delta{BaseDigest: vp.Current.Digest, Add: add})
		if err != nil {
			t.Fatal(err)
		}
		if rebuilt.Digest != vp.Next.Digest {
			t.Fatal("tag round-trip digest mismatch")
		}
	})
}

func TestAutoscale_NoDoublePlanner(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	n.machine.WriteCap = 100
	for i := 0; i < 80; i++ {
		n.noteZoneWrite("Demo000000000000", "aa")
	}
	dec := n.evaluateScaleDecision()
	if dec.Kind != KindHotSplit && dec.Kind != KindRegionalAdd {
		t.Fatalf("kind=%v", dec.Kind)
	}
	tags := n.preferNearsForLoadSplit(4, n.Name())
	if len(tags) == 0 {
		t.Fatal("topology plan must yield spawn tags, not a legacy empty fallback")
	}
	for _, tag := range tags {
		found := false
		for _, d := range dec.Plan.Add {
			if d.Name == tag {
				found = true
			}
		}
		if !found {
			t.Fatalf("tag %s is not in the validated plan (legacy load-split leaked)", tag)
		}
	}
}

func TestPLAN2_NoPlanDoesNotSpawnSplit(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	dec := n.evaluateScaleDecision()
	if dec.Kind != KindNone {
		t.Fatal("no write cap: topology planner must stay quiet")
	}
	got := n.preferNearsForLoadSplit(1, n.Name())
	if len(got) != 0 {
		t.Fatalf("no validated plan must not spawn, got %v", got)
	}
}

func TestSpawnPlanReservesWholeDecisionWithoutTruncation(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	state, err := n.planningState("")
	if err != nil {
		t.Fatal(err)
	}
	plan := TopologyPlan{BaseDigest: state.View.Digest, Donor: FromSlice(n.ID()), Reason: protocol.ReasonRegionalAdd}
	for _, bit := range []uint8{2, 0, 1} {
		id := tree.SetBit(plan.Donor, bit, 1-tree.Bit(plan.Donor, bit))
		plan.Add = append(plan.Add, VNodeDescriptor{ID: id, Name: EncodeName(id)})
	}
	validated, err := ValidatePlan(state.View, plan, state.Zones)
	if err != nil {
		t.Fatal(err)
	}
	before := state.View.Digest
	names := n.namesFromScaleState(Decision{Kind: KindRegionalAdd, Plan: plan}, 1, state)
	if len(names) != len(plan.Add) {
		t.Fatalf("truncated validated decision: %v, want %d additions", names, len(plan.Add))
	}
	pending, err := n.mesh.service.PendingSpawnNames()
	if err != nil || !slices.Equal(pending, names) {
		t.Fatalf("returned additions differ from committed reservation: names=%v pending=%v err=%v", names, pending, err)
	}
	var additions []VNodeDescriptor
	for i, name := range names {
		if name != plan.Add[i].Name {
			t.Fatal("spawn changed deterministic planner order")
		}
		id, err := tree.DecodeName(name)
		if err != nil {
			t.Fatal(err)
		}
		additions = append(additions, VNodeDescriptor{ID: id, Name: name})
	}
	rebuilt, err := tree.Apply(state.View, tree.Delta{BaseDigest: before, Add: additions})
	if err != nil || rebuilt.Digest != validated.Next.Digest {
		t.Fatalf("reserved names changed full-plan geometry: %v", err)
	}
	if n.topologyView().Digest != before {
		t.Fatal("reservation activated membership before an installed ticket")
	}
}
