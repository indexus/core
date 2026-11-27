package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// PlanningZones answers from the zone index: after the index is built, a
// call reads no row and no key, new zones and emptied zones included, where
// PlanningTotals reads every row. On a disk that owns all it holds the two
// agree on the number of zones.
func TestPlanningZonesReadsNoRow(t *testing.T) {
	const zones = 300
	store := sampleStore(t, zones)
	ctx := context.Background()
	agree := func(want int) {
		t.Helper()
		keys, _ := zonelog.ScanWork()
		scans, reads := rangeScans.Load(), pointReads.Load()
		got, err := store.PlanningZones(ctx)
		if err != nil || got != want {
			t.Fatalf("PlanningZones = %d, %v; want %d", got, err, want)
		}
		after, _ := zonelog.ScanWork()
		if after != keys || rangeScans.Load() != scans || pointReads.Load() != reads {
			t.Fatalf("PlanningZones examined %d keys in %d scans and %d point reads", after-keys, rangeScans.Load()-scans, pointReads.Load()-reads)
		}
		totals, err := store.PlanningTotals()
		if err != nil || totals.Zones != want {
			t.Fatalf("PlanningTotals counts %d zones, %v; want %d", totals.Zones, err, want)
		}
		if scanned, _ := zonelog.ScanWork(); scanned-after < int64(want) {
			t.Fatalf("PlanningTotals examined %d keys for %d zones: the comparison measures nothing", scanned-after, want)
		}
	}
	if _, err := store.PlanningZones(ctx); err != nil { // builds the index
		t.Fatal(err)
	}
	agree(zones)
	added := Record{Collection: "collection-00000", Location: "zzz", ID: "new", Metrics: []float64{1}}
	if _, err := store.Write("new-zone", added); err != nil {
		t.Fatal(err)
	}
	agree(zones + 1)
	z, _ := tree.NewZoneKey(added.Collection, added.Location)
	if err := store.update(func(tx *Tx) error { return tx.clearZone(z) }); err != nil {
		t.Fatal(err)
	}
	agree(zones)
	for i := range 3 {
		r := Record{Collection: "collection-00000", Location: "zzy", ID: fmt.Sprint("r", i), Metrics: []float64{1}}
		if _, err := store.Write(fmt.Sprint("same-zone-", i), r); err != nil {
			t.Fatal(err)
		}
	}
	agree(zones + 1)
}
