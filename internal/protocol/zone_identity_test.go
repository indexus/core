package protocol

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func coincidentAncestorZones(t testing.TB) []tree.ZoneKey {
	t.Helper()
	var zones []tree.ZoneKey
	for _, location := range []string{"0", "00", "000"} {
		zone, err := tree.NewZoneKey("0000000000000001", location)
		if err != nil {
			t.Fatal(err)
		}
		zones = append(zones, zone)
	}
	if zones[0].ID != zones[1].ID || zones[1].ID != zones[2].ID {
		t.Fatal("fixture must produce real coincident addresses")
	}
	return zones
}

func TestComponentKeepsCoincidentZoneMovesDistinct(t *testing.T) {
	zones := coincidentAncestorZones(t)
	old := mustView(t, tree.VNodeDescriptor{Name: "D", ID: tree.ID{255, 1}})
	component, err := NewComponent("aliases", old, tree.Delta{BaseDigest: old.Digest, Add: []tree.VNodeDescriptor{{Name: "J", ID: zones[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	component.Reconcile(zones)
	if len(component.Moves) != 3 {
		t.Fatalf("three zone identities produced %d moves", len(component.Moves))
	}
}

func TestValidatePlanKeepsCoincidentZoneStatistics(t *testing.T) {
	zones := coincidentAncestorZones(t)
	old := mustView(t, tree.VNodeDescriptor{Name: "D", ID: tree.ID{255, 1}})
	stats := []ZoneStats{{Zone: zones[0], Items: 2, Bytes: 20}, {Zone: zones[1], Items: 3, Bytes: 30}}
	plan, err := ValidatePlan(old, TopologyPlan{BaseDigest: old.Digest, Add: []tree.VNodeDescriptor{{Name: "J", ID: zones[0].ID}}}, stats)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 || plan.Changes[0].Items != 2 || plan.Changes[1].Items != 3 {
		t.Fatalf("statistics were overwritten by address: %+v", plan.Changes)
	}
}
