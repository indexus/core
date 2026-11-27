package mesh

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// REM-124: a fold whose survivor would own more than MaxItems is refused, so
// item-driven scaling does not split again what a fold just merged. The same
// pair folds once the bound allows it.
func TestCOLD2_FoldRespectsTheItemBound(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, nodes := meshFixture(t, 2)
	nodes[1].VNode.ID = tree.ID{0, 1}
	for i, location := range []string{"0", "00", "000", tree.EncodeName(tree.ID{192})} {
		r := storage.Record{Collection: "0000000000000001", Location: location, ID: fmt.Sprintf("row-%d", i), Metrics: []float64{1}}
		if _, err := nodes[0].Write(ctx, "seed-"+r.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].Sync(ctx, []string{nodes[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	if err := nodes[1].Compact(ctx, ColdConfig{MaxItems: 3}); !errors.Is(err, storage.ErrDataCapacity) {
		t.Fatalf("a fold over the item bound: %v, want ErrDataCapacity", err)
	}
	if pin, _ := nodes[0].Store.ColdStatus(); pin.Pin != nil {
		t.Fatal("a refused fold left the survivor pinned")
	}
	if err := nodes[1].Compact(ctx, ColdConfig{MaxItems: 4}); err != nil {
		t.Fatalf("a fold within the item bound: %v", err)
	}
	if status, err := nodes[1].Store.ColdStatus(); err != nil || status.Drain == nil || !status.Drain.Witnessed {
		t.Fatalf("fold within the bound not finished: %+v %v", status, err)
	}
}
