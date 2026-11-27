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

// coldPair writes rows on the root, joins the second vnode and returns the
// expected content. nodes[1] is the fold source, nodes[0] its survivor.
func coldPair(t *testing.T, ctx context.Context, rows int) (*network, []*Service, map[string]storage.Record) {
	t.Helper()
	net, nodes, _ := coldRestartFixture(t)
	expected := map[string]storage.Record{}
	for i := 0; i < rows; i++ {
		row := storage.Record{Collection: "0000000000000001", Location: "00", ID: fmt.Sprint("row", i), Metrics: []float64{float64(i)}}
		if i%2 == 1 {
			row.Location = tree.EncodeName(tree.ID{1})
		}
		if _, err := nodes[0].Write(ctx, row.ID, row); err != nil {
			t.Fatal(err)
		}
		expected[row.Location+":"+row.ID] = row
	}
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	// Settle both ways so the survivor has collected what it handed off and
	// its usage counters are what a later admission will read.
	for range 2 {
		for i, node := range nodes {
			if err := node.Sync(ctx, []string{nodes[1-i].Self.Host}); err != nil {
				t.Fatal(err)
			}
			if err := node.Resume(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	return net, nodes, expected
}

// COLD-2: once every piece is installed, heat no longer preempts. Writes over
// the budget on both sides while the last piece moves: the fold completes,
// the source retires and every row, hot ones included, stays readable.
func TestCOLD2_HeatDuringLastPieceCompletesFold(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	net, nodes, expected := coldPair(t, ctx, 6)
	heated := false
	net.setAfter(func(req Request) error {
		if req.Kind != "preload" || heated {
			return nil
		}
		heated = true
		for i := 0; i < 3; i++ {
			for side, writer := range nodes {
				hot := storage.Record{Collection: "0000000000000001", Location: "000", ID: fmt.Sprintf("hot-%d-%d", side, i)}
				if side == 0 {
					hot.Location = tree.EncodeName(tree.ID{1})
				}
				if _, _, err := writer.Store.Apply(hot.ID, hot); err != nil {
					return err
				}
				expected[hot.Location+":"+hot.ID] = hot
			}
		}
		return nil
	})
	if err := nodes[1].Compact(ctx, ColdConfig{HeatBudget: 1}); err != nil {
		t.Fatalf("heat after the pieces started preempted the fold: %v", err)
	}
	net.setAfter(nil)
	if !heated {
		t.Fatal("heat not exercised")
	}
	snapshot, err := nodes[0].Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Current().HasName(nodes[1].Self.VNode) {
		t.Fatal("source not retired")
	}
	assertColdRows(t, ctx, nodes, expected, false)
	// Nothing travels back: the retired source holds no grant to reclaim.
	if err := nodes[0].Rebalance(ctx); err != nil {
		t.Fatal(err)
	}
	if err := nodes[1].Rebalance(ctx); err != nil {
		t.Fatal(err)
	}
	inventory, err := nodes[1].Store.Inventory(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, snap := range inventory {
		for _, r := range snap.Records {
			if !r.Deleted {
				t.Fatalf("retired source still owns %s:%s", snap.Zone.Location, r.ID)
			}
		}
	}
}

// COLD-2 admission is exact at the boundary: owned(source)+owned(survivor)
// against fraction × survivor capacity. A refusal leaves both sides unsealed
// and the source writable.
func TestCOLD2_SurvivorFractionBoundary(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, nodes, expected := coldPair(t, ctx, 8)
	capacity, survivor, err := nodes[0].Store.CapacityUsage()
	if err != nil {
		t.Fatal(err)
	}
	_, source, err := nodes[1].Store.CapacityUsage()
	if err != nil {
		t.Fatal(err)
	}
	if source.Owned == 0 || survivor.Owned == 0 {
		t.Fatalf("both sides must own data: %d %d", source.Owned, survivor.Owned)
	}
	exact := float64(source.Owned+survivor.Owned) / float64(capacity)
	if err := nodes[1].Compact(ctx, ColdConfig{MaxCombinedFraction: exact * 0.999}); !errors.Is(err, storage.ErrDataCapacity) {
		t.Fatalf("fold above the fraction admitted: %v", err)
	}
	src, _ := nodes[1].Store.ColdStatus()
	dst, _ := nodes[0].Store.ColdStatus()
	if src.Drain != nil || dst.Pin != nil {
		t.Fatal("refused fold left seals")
	}
	probe := storage.Record{Collection: "0000000000000001", Location: "00", ID: "after-refusal"}
	if _, _, err := nodes[1].Store.Apply(probe.ID, probe); err != nil {
		t.Fatalf("source not writable after refusal: %v", err)
	}
	expected[probe.Location+":"+probe.ID] = probe
	// The probe grew the source; admit with a margin above the new total.
	_, source, _ = nodes[1].Store.CapacityUsage()
	exact = float64(source.Owned+survivor.Owned) / float64(capacity)
	if err := nodes[1].Compact(ctx, ColdConfig{MaxCombinedFraction: exact * 1.001}); err != nil {
		t.Fatalf("fold under the fraction refused: %v", err)
	}
	assertColdRows(t, ctx, nodes, expected, false)
}

// Heat under the budget before any piece starts does not preempt: a tepid
// vnode still folds. Over the budget it does (TestColdFoldHeatPreempts...).
func TestCOLD2_HeatUnderBudgetStillFolds(t *testing.T) {
	t.Parallel()
	for _, side := range []int{0, 1} {
		t.Run(fmt.Sprint("writer-", side), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			net, nodes, expected := coldPair(t, ctx, 4)
			heated := false
			net.setAfter(func(req Request) error {
				if req.Kind != KindColdPin || heated {
					return nil
				}
				heated = true
				for i := 0; i < 3; i++ {
					warm := storage.Record{Collection: "0000000000000001", Location: "00", ID: fmt.Sprint("warm", i)}
					if side == 0 {
						warm.Location = tree.EncodeName(tree.ID{1})
					}
					if _, _, err := nodes[side].Store.Apply(warm.ID, warm); err != nil {
						return err
					}
					expected[warm.Location+":"+warm.ID] = warm
				}
				return nil
			})
			if err := nodes[1].Compact(ctx, ColdConfig{HeatBudget: 3}); err != nil {
				t.Fatalf("three writes within a budget of three preempted: %v", err)
			}
			net.setAfter(nil)
			if !heated {
				t.Fatal("heat not exercised")
			}
			assertColdRows(t, ctx, nodes, expected, false)
		})
	}
}
