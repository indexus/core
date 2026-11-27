package mesh

import (
	"context"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// U4 against a real view: after joins, R(z) on every node is the owner then
// the next closest vnodes, one per machine, and agrees across nodes.
func TestREPL1_ReplicaSetOnTheRoutingView(t *testing.T) {
	t.Parallel()
	_, nodes := meshFixture(t, 4)
	ctx := context.Background()
	for _, n := range nodes[1:] {
		if err := n.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var hosts []string
	for _, n := range nodes {
		hosts = append(hosts, n.Self.Host)
	}
	for _, n := range nodes {
		if err := n.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
	}
	byHost := func(h causal.Endpoint) string { return h.Host }
	oneMachine := func(causal.Endpoint) string { return "same" }
	first, err := nodes[0].Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(first.Current().Active); n != len(nodes) {
		t.Fatalf("view has %d active vnodes, want %d", n, len(nodes))
	}
	for i := range 64 {
		zone, err := tree.NewZoneKey(fmt.Sprintf("Collection%06d", i), "aa")
		if err != nil {
			t.Fatal(err)
		}
		want := replicas(first, zone, byHost)
		if len(want) != 3 {
			t.Fatalf("zone %d: R(z) has %d members, want 3", i, len(want))
		}
		owner, err := first.Route(zone.ID)
		if err != nil || want[0] != owner {
			t.Fatalf("zone %d: R(z)[0] = %v, owner %v (%v)", i, want[0], owner, err)
		}
		seen := map[string]bool{}
		for _, h := range want {
			if seen[h.Host] {
				t.Fatalf("zone %d: two members on %s", i, h.Host)
			}
			seen[h.Host] = true
		}
		if got := replicas(first, zone, oneMachine); len(got) != 1 || got[0] != owner {
			t.Fatalf("zone %d: one machine gives %v, want the owner only", i, got)
		}
		for _, n := range nodes[1:] {
			routing, err := n.Store.CausalRouting()
			if err != nil {
				t.Fatal(err)
			}
			if got := replicas(routing, zone, byHost); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("zone %d: %s has R(z) %v, %s has %v", i, n.VNode.Name, got, nodes[0].VNode.Name, want)
			}
		}
	}
}
