package mesh

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// REM-121: a split's plan listed every moving zone in Changes, and the durable
// intent carried it to every node (12 MB in a local spike). Validation
// recomputes changes from the view and only the author reads intents, so a
// reserved intent keeps the decision and drops the per-zone forecast.
func TestReservedIntentCarriesNoZoneForecast(t *testing.T) {
	t.Parallel()
	_, nodes := meshFixture(t, 2)
	author, joiner := nodes[0], nodes[1]
	replica, err := author.Membership()
	if err != nil {
		t.Fatal(err)
	}
	plan := protocol.TopologyPlan{BaseDigest: replica.Current().Digest, Reason: protocol.ReasonRegionalAdd, Add: []tree.VNodeDescriptor{joiner.VNode}}
	for _, loc := range []string{"W", "X", "m", "n"} {
		zone, err := tree.NewZoneKey("Probe00000000000", loc)
		if err != nil {
			t.Fatal(err)
		}
		plan.Changes = append(plan.Changes, protocol.ZoneChange{Zone: zone, OldOwner: author.VNode.ID, NewOwner: joiner.VNode.ID, Items: 1000})
	}
	if err := author.Reserve(plan); err != nil {
		t.Fatal(err)
	}
	names, err := author.PendingSpawnNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != joiner.VNode.Name {
		t.Fatalf("decision lost: %v", names)
	}
	_, _, delta, err := author.Store.Membership()
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Members) != 1 {
		t.Fatal("pending spawn/forecast leaked into gossip")
	}
}
