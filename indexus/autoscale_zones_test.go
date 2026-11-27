package indexus

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// An autoscale tick counts its zones from the zone index: after a write it
// examines no key of the store, where OwnershipStats reads every row again
// (14 % of a donor's CPU on AWS on 2026-11-26). Not parallel: the counter of
// examined keys is the process's.
func TestAutoscaleTickCountsZonesWithoutScanning(t *testing.T) {
	n := ownedNode(t)
	write := func(i int) {
		t.Helper()
		r := storage.Record{Collection: "Demo000000000000", Location: fmt.Sprintf("a%c", 'a'+i), ID: "r", Metrics: []float64{1}}
		if _, err := n.durableStore().Write(fmt.Sprint("tick-", i), r); err != nil {
			t.Fatal(err)
		}
	}
	write(0)
	n.retainedOwnedMetrics() // builds the index
	for i := 1; i <= 5; i++ {
		write(i)
		before, _ := zonelog.ScanWork()
		zones, _ := n.retainedOwnedMetrics()
		if after, _ := zonelog.ScanWork(); after != before {
			t.Fatalf("tick %d examined %d keys of the store", i, after-before)
		}
		if _, owned := n.OwnershipStats(); zones != owned {
			t.Fatalf("tick %d counts %d zones, the scan %d", i, zones, owned)
		}
	}
}
