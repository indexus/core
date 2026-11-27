package indexus

import (
	"testing"
)

func TestScalePlanValidatesThenReservesWholeDecision(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	n.machine.WriteCap = 100
	for i := 0; i < 80; i++ {
		n.noteZoneWrite("Demo000000000000", "aa")
	}
	dec := n.evaluateScaleDecision()
	if dec.Kind != KindHotSplit && dec.Kind != KindRegionalAdd {
		t.Fatalf("kind=%v err=%v", dec.Kind, dec.Err)
	}
	vp, err := ValidatePlan(n.topologyView(), dec.Plan, mustZoneStats(t, n))
	if err != nil {
		t.Fatal(err)
	}
	names := n.namesFromScalePlan(dec, 2)
	if len(names) != len(vp.Plan.Add) {
		t.Fatal("validated plan produced no PreferNear names")
	}
}

func TestPreferNearUsesValidatedForkNotHeatPrefix(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	n.machine.WriteCap = 100
	for i := 0; i < 80; i++ {
		n.noteZoneWrite("Demo000000000000", "aa")
	}
	got := n.preferNearsForLoadSplit(1, n.Name())
	if len(got) != 1 || got[0] == "" {
		t.Fatalf("prefer_nears=%v", got)
	}
	if got[0] == n.Name() {
		t.Fatal("spawn tag must not be the donor name")
	}
}

// These helpers sample the production planner for assertions only.
func (n *Node) evaluateScaleDecision() Decision {
	st, err := n.planningState("")
	if err != nil {
		return Decision{Err: err}
	}
	return EvaluateMachine(st)
}

func (n *Node) preferNearsForLoadSplit(spawnN int, fallback string) []string {
	return n.preferNearsForDemand("", spawnN, fallback)
}
func (n *Node) namesFromScalePlan(d Decision, spawnN int) []string {
	st, err := n.planningState("")
	if err != nil {
		return nil
	}
	return n.namesFromScaleState(d, spawnN, st)
}
