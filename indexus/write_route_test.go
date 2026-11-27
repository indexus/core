package indexus

import (
	"context"
	"testing"
	"time"
)

// A write sent to a node that does not own its zone is relayed, and its
// answer names the owner and the owner's cell, which holds the zone; a
// local ACK must teach the same actual piece, including before membership arrives.
func TestRelayedWriteNamesItsOwnerAndCell(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	collection := "0000000000000001"
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = BASE64.Encode(root.ID.Bytes())
	zone, _ := NewZoneKey(collection, "0")
	target := VNodeDescriptor{ID: zone.ID}
	target.Name = BASE64.Encode(target.ID.Bytes())
	fixture := newReadPhaseFixture(t, []VNodeDescriptor{root, target}, false, false)
	nodes, services := fixture.nodes, fixture.services
	if err := services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if err := n.Update(); err != nil {
			t.Fatal(err)
		}
	}
	item := &Item{Collection: collection, Location: "0", Id: "routed", Metrics: []float64{1}}
	route, err := nodes[0].NewRouted(item, BASE64.Root(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if route.Owner != services[1].Self.Host {
		t.Fatalf("relayed write names owner %q, want %q", route.Owner, services[1].Self.Host)
	}
	cell, err := DecodeCell(route.Cell)
	if err != nil {
		t.Fatal(err)
	}
	if !cell.Contains(zone.ID) {
		t.Fatalf("the owner's cell %s does not hold the zone written", route.Cell)
	}
	item.Id = "direct"
	route, err = nodes[1].NewRouted(item, BASE64.Root(), nil)
	if err != nil || route.Owner != services[1].Self.Host || route.Cell == "" {
		t.Fatalf("a write applied where it was sent named %+v (%v)", route, err)
	}
}

func TestLocalACKDoesNotAdvertiseTheoreticalMembershipCell(t *testing.T) {
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = BASE64.Encode(root.ID.Bytes())
	target := VNodeDescriptor{ID: ID{1}}
	target.Name = BASE64.Encode(target.ID.Bytes())
	f := newReadPhaseFixture(t, []VNodeDescriptor{root, target}, false, false)
	// Membership still advertises the original root. Its ACK must name itself
	// and a locally installed region, rather than omitting all routing data.
	item := &Item{Collection: "0000000000000001", Location: "0", Id: "before-gossip", Metrics: []float64{1}}
	route, err := f.nodes[0].NewRouted(item, BASE64.Root(), nil)
	if err != nil || route.Owner != f.services[0].Self.Host || route.Cell == "" {
		t.Fatalf("local ACK lost route: %+v %v", route, err)
	}
}
