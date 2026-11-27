package indexus

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestSCALEWIRE1_AutoscaleChangeComesFromValidatedPlan(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	n.machine.WriteCap = 100
	for i := 0; i < 80; i++ {
		n.noteZoneWrite("Demo000000000000", "aa")
	}
	before := n.topologyView()
	digest, gen := before.Digest, before.Generation
	dec := n.evaluateScaleDecision()
	if n.topologyView().Digest != digest || n.topologyView().Generation != gen {
		t.Fatal("EvaluateMachine changed V_active")
	}
	if dec.Kind != KindHotSplit && dec.Kind != KindRegionalAdd {
		t.Fatalf("kind=%v err=%v", dec.Kind, dec.Err)
	}
	zones := mustZoneStats(t, n)
	vp, err := ValidatePlan(n.topologyView(), dec.Plan, zones)
	if err != nil {
		t.Fatal(err)
	}
	tags := n.preferNearsForLoadSplit(len(vp.Plan.Add)+1, n.Name())
	if n.topologyView().Digest != digest {
		t.Fatal("naming spawn tags changed V_active")
	}
	if len(tags) == 0 {
		t.Fatal("validated plan produced no handoff names")
	}
	for _, tag := range tags {
		found := false
		for _, d := range vp.Plan.Add {
			if d.Name == tag {
				found = true
			}
		}
		if !found {
			t.Fatalf("tag %s is not in the validated plan", tag)
		}
	}
	if n.topologyView().Digest != digest || n.topologyView().Generation != gen {
		t.Fatal("the autoscale naming path published V_active itself")
	}
}

func TestSCALEWIRE1_UnvalidatedPlanDoesNotChangeActiveSet(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	digest := n.topologyView().Digest
	dec := Decision{
		Kind: KindHotSplit,
		Plan: TopologyPlan{
			BaseDigest: [32]byte{9},
			Donor:      FromSlice(n.ID()),
			Reason:     ReasonHotSplit,
			Add:        []VNodeDescriptor{{ID: tree.SetBit(ID{}, 0, 1), Name: "J"}},
		},
	}
	if tags := n.namesFromScalePlan(dec, 1); len(tags) != 0 {
		t.Fatalf("unvalidated plan spawned %v", tags)
	}
	if n.topologyView().Digest != digest {
		t.Fatal("rejected plan changed V_active")
	}
}
