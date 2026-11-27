package indexus

import (
	"context"
	"github.com/indexus/go-indexus-core/internal/summary"
	"testing"
	"time"
)

func TestObserveDefersAdvisoryCountDuringWrites(t *testing.T) {
	t.Parallel()
	n := filledNode(t, []string{"aa"}, 2)
	if err := n.Observe(); err != nil {
		t.Fatal(err)
	}
	if n.Items() != 2 {
		t.Fatal("initial count was deferred")
	}
	if err := n.New(&Item{Collection: "Demo000000000000", Location: "ab", Id: "new", Metrics: []float64{7}}, "@", nil); err != nil {
		t.Fatal(err)
	}
	if err := n.Observe(); err != nil {
		t.Fatal(err)
	}
	if n.Items() != 2 {
		t.Fatal("active writer triggered inventory recount")
	}
	n.mesh.lastWrite.Store(time.Now().Add(-2 * time.Second).UnixNano())
	if err := n.Observe(); err != nil {
		t.Fatal(err)
	}
	if n.Items() != 3 {
		t.Fatal("quiet recount did not catch up")
	}
}

func TestPeriodicFoldDefersWhileDeltasRemainCurrent(t *testing.T) {
	t.Parallel()
	n := filledNode(t, []string{"aa"}, 2)
	if _, err := n.mesh.summary.Fold(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	n.mesh.summary.LastFold = time.Now().Add(-2 * summary.InventoryEvery)
	if err := n.New(&Item{Collection: "Demo000000000000", Location: "ab", Id: "new", Metrics: []float64{7}}, "@", nil); err != nil {
		t.Fatal(err)
	}
	if ran, err := n.mesh.summary.Fold(context.Background(), false); err != nil || ran {
		t.Fatalf("busy fold: %t %v", ran, err)
	}
	if got := n.mesh.summary.Owned("Demo000000000000", "@"); got.Count() != 3 {
		t.Fatal("incremental summaries stopped", got)
	}
	// Explicit repair is still available even during ongoing writes.
	if ran, err := n.mesh.summary.Fold(context.Background(), true); err != nil || !ran {
		t.Fatalf("forced fold: %t %v", ran, err)
	}
}
