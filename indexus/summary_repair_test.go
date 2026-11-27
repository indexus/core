package indexus

import (
	"context"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/summary"
	"testing"
	"time"
)

func TestUnchangedInventoryDoesNotRebuildOnExpiredTimer(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 5000)
	if _, err := n.mesh.summary.Fold(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	n.mesh.summary.LastFold = time.Now().Add(-2 * summary.InventoryEvery)
	if ran, err := n.mesh.summary.Fold(context.Background(), false); err != nil || ran {
		t.Fatalf("unchanged rescan: %t %v", ran, err)
	}
	if _, _, _, err := n.durableStore().ApplyRecord("revision-write", storage.Record{Collection: "Demo000000000000", Location: "abc", ID: "new", Metrics: []float64{7}}); err != nil {
		t.Fatal(err)
	}
	if ran, err := n.mesh.summary.Fold(context.Background(), false); err != nil || !ran {
		t.Fatalf("changed inventory skipped: %t %v", ran, err)
	}
	if got := n.mesh.summary.Owned("Demo000000000000", "@"); got.Count() != 1 || got.Metric(0) != 7 {
		t.Fatal(got)
	}
}
