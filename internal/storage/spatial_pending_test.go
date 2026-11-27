package storage

import (
	"fmt"
	"testing"
)

func TestSpatialColdPendingTracksZonesInsteadOfWriteHistory(t *testing.T) {
	store, _ := routingStore(t)
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "hot"}
	zone, _ := recordZone(r)
	// Hold the cold builder at its publication boundary deterministically.
	// Actual writer commits below must retain only each zone's final presence.
	store.spatial.mu.Lock()
	store.spatial.state.Store(nil)
	store.spatial.building = true
	store.spatial.pending = nil
	store.spatial.mu.Unlock()
	defer func() {
		store.spatial.mu.Lock()
		store.spatial.building = false
		store.spatial.pending = nil
		store.spatial.mu.Unlock()
	}()
	check := func(want bool) {
		t.Helper()
		store.spatial.mu.Lock()
		defer store.spatial.mu.Unlock()
		if len(store.spatial.pending) != 1 || store.spatial.pending[zone] != want {
			t.Fatalf("pending must retain one final presence, got %+v", store.spatial.pending)
		}
	}
	for i := range 64 {
		r.Metrics = []float64{float64(i)}
		if _, err := store.Write(fmt.Sprintf("cold-write-%d", i), r); err != nil {
			t.Fatal(err)
		}
		check(true)
	}
	if err := store.update(func(tx *Tx) error { return tx.clearZone(zone) }); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := store.Write("cold-reinsert", r); err != nil {
		t.Fatal(err)
	}
	check(true)
	// A second zone consumes one additional entry; repeated overwrites do not.
	r.Location = "bb"
	second, _ := recordZone(r)
	if _, err := store.Write("second-zone", r); err != nil {
		t.Fatal(err)
	}
	store.spatial.mu.Lock()
	defer store.spatial.mu.Unlock()
	if len(store.spatial.pending) != 2 || !store.spatial.pending[second] {
		t.Fatal("pending is not bounded by distinct zones")
	}
}
