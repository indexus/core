package indexus

import (
	"fmt"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"testing"
)

func TestMonitoringPlanningParityIncludingFrozenSource(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	for i, location := range []string{"aa", "aa0", "bb"} {
		for j := range 3 {
			record := storage.Record{Collection: "Demo000000000000", Location: location, ID: fmt.Sprintf("row-%d", j), Deleted: j == 2, Metrics: []float64{float64(i + j)}}
			if _, _, _, err := n.durableStore().ApplyRecord(fmt.Sprintf("monitor-%d-%d", i, j), record); err != nil {
				t.Fatal(err)
			}
		}
	}
	check := func() {
		t.Helper()
		inventory, err := n.durableStore().Inventory(false)
		if err != nil {
			t.Fatal(err)
		}
		want, collections := 0, map[string]bool{}
		for _, snap := range inventory {
			collections[snap.Zone.Collection] = true
			for _, r := range snap.Records {
				if !r.Deleted {
					want++
				}
			}
		}
		count, err := n.Count()
		if err != nil || count != want || n.Items() != want {
			t.Fatalf("count=%d cached=%d want=%d err=%v", count, n.Items(), want, err)
		}
		c, z := n.OwnershipStats()
		if c != len(collections) || z != len(inventory) {
			t.Fatalf("ownership=%d/%d want=%d/%d", c, z, len(collections), len(inventory))
		}
		ownership, err := n.Ownership()
		if err != nil {
			t.Fatal(err)
		}
		zones := 0
		for _, locations := range ownership {
			zones += len(locations)
		}
		if len(ownership) != c || zones != z {
			t.Fatal("ownership surfaces disagree")
		}
	}
	check()
	// Frozen source retains physical rows but must report no writable ownership,
	// like PlanningStats rather than raw live-bucket counts. Client reads still
	// serve those rows (storage readableIn), so Inventory is no reference here.
	freezeWithConfirmedReceiver(t, n.durableStore(), causal.Endpoint{Host: "receiver-host", Owner: causal.Owner{VNode: "receiver"}})
	if count, err := n.Count(); err != nil || count != 0 {
		t.Fatalf("frozen source=%d %v", count, err)
	}
}

func TestMeasuredOwnershipStatsDoNotScanOrRefreshOnRead(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	if c, z := n.MeasuredOwnershipStats(); c != 0 || z != 0 {
		t.Fatal("measurement must start empty")
	}
	write := func(collection, location, id string) {
		t.Helper()
		if _, err := n.durableStore().Write(id, storage.Record{Collection: collection, Location: location, ID: id, Metrics: []float64{1}}); err != nil {
			t.Fatal(err)
		}
	}
	initialCollections, initialZones := n.OwnershipStats()
	write("First00000000000", "aa", "one")
	if _, err := n.Count(); err != nil {
		t.Fatal(err)
	}
	write("Second0000000000", "bb", "two")
	// Fresh planning can see the mutation; observing a cached status must not.
	if c, z := n.OwnershipStats(); c != initialCollections+2 || z != initialZones+2 {
		t.Fatalf("fresh planning=%d/%d", c, z)
	}
	for range 100 {
		if c, z := n.MeasuredOwnershipStats(); c != initialCollections+1 || z != initialZones+1 {
			t.Fatalf("poll unexpectedly refreshed=%d/%d", c, z)
		}
	}
	if _, err := n.Count(); err != nil {
		t.Fatal(err)
	}
	if c, z := n.MeasuredOwnershipStats(); c != initialCollections+2 || z != initialZones+2 {
		t.Fatalf("measurement not published=%d/%d", c, z)
	}
}
