package mesh

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// ROUTE-REGION-1: the most specific learned region wins, a hint that proved
// wrong is dropped alone, and the cache stays bounded.
func TestRegionRoutesPreferTheMostSpecificRegion(t *testing.T) {
	t.Parallel()
	end := func(name string) causal.Endpoint {
		return causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: name}, Host: name}
	}
	half := tree.Region{Mask: tree.ID{0x80}, Value: tree.ID{0x80}}
	quarter := tree.Region{Mask: tree.ID{0xC0}, Value: tree.ID{0xC0}}
	var r regionRoutes
	r.put(half, end("a"))
	r.put(quarter, end("b"))
	if _, to, ok := r.get(tree.ID{0xF0}); !ok || to != end("b") {
		t.Fatalf("address in the quarter routed to %v, want b", to)
	}
	if _, to, ok := r.get(tree.ID{0x90}); !ok || to != end("a") {
		t.Fatalf("address in the half only routed to %v, want a", to)
	}
	if _, _, ok := r.get(tree.ID{0x10}); ok {
		t.Fatal("address outside every learned region got a hint")
	}
	r.drop(quarter, end("a")) // names another holder: kept
	if _, to, _ := r.get(tree.ID{0xF0}); to != end("b") {
		t.Fatal("drop removed a hint naming another holder")
	}
	r.drop(quarter, end("b"))
	if _, to, ok := r.get(tree.ID{0xF0}); !ok || to != end("a") {
		t.Fatalf("after dropping the quarter, routed to %v, want the half's a", to)
	}
	r.put(half, end("c"))
	if _, to, _ := r.get(tree.ID{0x90}); to != end("c") || len(r.entries) != 2 {
		t.Fatalf("relearning a region must replace it: %v, %d entries", to, len(r.entries))
	}
	for i := 0; i < 3*regionRouteLimit; i++ {
		r.put(tree.Region{Mask: tree.ID{0xFF, 0xFF, 0xFF}, Value: tree.ID{byte(i), byte(i >> 8), 1}}, end("d"))
	}
	if len(r.entries) != regionRouteLimit {
		t.Fatalf("region hints unbounded: %d", len(r.entries))
	}
}

// ROUTE-REGION-1 on the write path: the entry learns the applier's region
// from the ACK, a wrong regional hint costs no ACK and is replaced.
func TestEntryLearnsRegionFromAckAndDropsWrongHint(t *testing.T) {
	t.Parallel()
	_, nodes := meshFixture(t, 2)
	ctx := context.Background()
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		for _, node := range nodes {
			if err := node.Sync(ctx, []string{nodes[0].Self.Host, nodes[1].Self.Host}); err != nil {
				t.Fatal(err)
			}
			if err := node.Resume(ctx); err != nil {
				t.Fatal(err)
			}
			if err := node.Rebalance(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	entry := nodes[0]
	hint := func(id tree.ID) (causal.Endpoint, bool) {
		entry.routeMu.Lock()
		defer entry.routeMu.Unlock()
		_, to, ok := entry.regions.get(id)
		return to, ok
	}
	write := func(i int, name string) (tree.ZoneKey, Response, bool) {
		record := storage.Record{Collection: tree.EncodeName(tree.ID{byte(i), 1}), Location: tree.BASE64.Root(), ID: name}
		zone, err := tree.NewZoneKey(record.Collection, record.Location)
		if err != nil {
			t.Fatal(err)
		}
		holder, err := entry.Store.ZoneHolder(zone)
		if err != nil {
			t.Fatal(err)
		}
		if !holder.Valid() || holder.Owner == entry.Self.Owner {
			return zone, Response{}, false
		}
		out, err := entry.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("%s-%d", name, i), Record: &record})
		if err != nil {
			t.Fatal(err)
		}
		return zone, out, true
	}
	var zone tree.ZoneKey
	found := -1
	for i := 1; i < 256 && found < 0; i++ {
		z, out, relayed := write(i, "first")
		if !relayed {
			continue
		}
		if out.AppliedBy != nodes[1].Self || out.AppliedRegion == nil || !out.AppliedRegion.Contains(z.ID) {
			t.Fatalf("relayed ACK by %v with region %v", out.AppliedBy, out.AppliedRegion)
		}
		zone, found = z, i
	}
	if found < 0 {
		t.Fatal("no zone held by the joined node")
	}
	if to, ok := hint(zone.ID); !ok || to != nodes[1].Self {
		t.Fatalf("entry learned %v (%v) for the ACKed region, want %v", to, ok, nodes[1].Self)
	}
	// Poison the cache: the most specific region around the zone now names
	// the entry itself, which no longer holds it.
	exact := tree.Region{Value: zone.ID}
	for i := range exact.Mask {
		exact.Mask[i] = 0xFF
	}
	entry.routeMu.Lock()
	entry.routes = zoneRoutes{}
	entry.regions.put(exact, entry.Self)
	entry.routeMu.Unlock()
	if _, out, relayed := write(found, "second"); !relayed || out.AppliedBy != nodes[1].Self {
		t.Fatalf("write behind a wrong hint applied by %v", out.AppliedBy)
	}
	entry.routeMu.Lock()
	region, to, _ := entry.regions.get(zone.ID)
	entry.routeMu.Unlock()
	if region == exact || to != nodes[1].Self {
		t.Fatalf("wrong hint kept: %v -> %v", region, to)
	}
}

func TestRegionRoutesBoundRetainedEndpointBytes(t *testing.T) {
	var r regionRoutes
	endpoint := causal.Endpoint{Owner: causal.Owner{VNode: "v", Storage: "disk"}, Host: strings.Repeat("h", regionRouteTextBytes/2)}
	for i := range 8 {
		region := tree.Region{Mask: tree.ID{0xff}, Value: tree.ID{byte(i)}}
		r.put(region, endpoint)
		actual := 0
		for _, entry := range r.entries {
			actual += endpointTextBytes(entry.holder)
		}
		if actual != r.textBytes || actual > regionRouteTextBytes {
			t.Fatalf("retained bytes=%d counter=%d", actual, r.textBytes)
		}
	}
	if cap(r.entries) != regionRouteLimit {
		t.Fatalf("unexpected backing capacity %d", cap(r.entries))
	}
	region := tree.Region{Mask: tree.ID{0xff}, Value: tree.ID{7}}
	if _, _, ok := r.get(region.Value); !ok {
		t.Fatal("latest bounded hint was evicted")
	}
	r.drop(region, endpoint)
	if r.textBytes != 0 {
		t.Fatalf("dropped endpoint retains %d bytes", r.textBytes)
	}
	endpoint.Host = strings.Repeat("h", regionRouteTextBytes+1)
	r.put(region, endpoint)
	if r.textBytes != 0 {
		t.Fatal("oversized hint was retained")
	}
}
