package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestInventoryPagesPreserveCanonicalPrefixAndRejectChangedOrUnknownCursor(t *testing.T) {
	store, _ := routingStore(t)
	const collection = "0000000000000001"
	for _, key := range []tree.Key{{Collection: collection, Location: "0"}, {Collection: collection, Location: "00"}, {Collection: collection, Location: "000"}, {Collection: collection, Location: "01"}, {Collection: "0000000000000000", Location: "00"}} {
		for i := 0; i < 3; i++ {
			record := Record{Collection: key.Collection, Location: key.Location, ID: fmt.Sprint(i), Metrics: []float64{float64(i)}}
			if _, _, _, err := store.ApplyRecord(key.Collection+key.Location+record.ID, record); err != nil {
				t.Fatal(err)
			}
		}
	}
	parent, _ := tree.NewZoneKey(collection, "0")
	child, _ := tree.NewZoneKey(collection, "00")
	if parent.ID != child.ID {
		t.Fatal("fixture needs coincident parent/child IDs")
	}
	request := InventoryPageRequest{ByteLimit: 1}
	seen := map[string]bool{}
	pages := 0
	var first *InventoryCursor
	for {
		rows, page, err := store.CausalInventoryPage(context.Background(), &parent, request)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, snapshot := range rows {
			for _, record := range snapshot.Records {
				key := record.Location + ":" + record.ID
				if record.Collection != collection || seen[key] {
					t.Fatalf("wrong/duplicate record: %+v", record)
				}
				seen[key] = true
			}
		}
		if first == nil && page.Next != nil {
			copy := *page.Next
			first = &copy
		}
		if page.Done {
			if page.Next != nil {
				t.Fatal("done with cursor")
			}
			break
		}
		if page.Next == nil || page.Watermark == 0 {
			t.Fatal("incomplete page has no valid cursor")
		}
		request.After = page.Next
	}
	if len(seen) != 12 || pages != 12 {
		t.Fatalf("rows=%d pages=%d", len(seen), pages)
	}
	for _, change := range []func(*InventoryCursor){func(c *InventoryCursor) { c.RecordID = "missing" }, func(c *InventoryCursor) { c.Location = "z" }, func(c *InventoryCursor) { c.Watermark++ }} {
		invalid := *first
		change(&invalid)
		rows, _, err := store.CausalInventoryPage(context.Background(), &parent, InventoryPageRequest{After: &invalid, ByteLimit: 1})
		if !errors.Is(err, ErrReadChanged) || rows != nil {
			t.Fatalf("unknown cursor exposed partial result: %v %v", rows, err)
		}
	}
	if _, _, _, err := store.ApplyRecord("after-page", Record{Collection: collection, Location: "00", ID: "new"}); err != nil {
		t.Fatal(err)
	}
	if rows, _, err := store.CausalInventoryPage(context.Background(), &parent, InventoryPageRequest{After: first}); !errors.Is(err, ErrReadChanged) || rows != nil {
		t.Fatalf("changed snapshot accepted: %v", err)
	}
	absent, _ := tree.NewZoneKey(collection, "z")
	rows, page, err := store.CausalInventoryPage(context.Background(), &absent, InventoryPageRequest{})
	if err != nil || len(rows) != 0 || !page.Done || page.Next != nil || page.Watermark == 0 {
		t.Fatalf("empty completion: %+v %+v %v", rows, page, err)
	}
}

type cancelDuringScan struct {
	context.Context
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (c *cancelDuringScan) Err() error {
	if c.calls.Add(1) == 8 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestInventoryScansStopOnCancellationWithoutPartialSuccess(t *testing.T) {
	store, _ := routingStore(t)
	for i := 0; i < 32; i++ {
		if _, _, _, err := store.ApplyRecord(fmt.Sprint(i), Record{Collection: "Collection000000", Location: "a", ID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	zone, _ := tree.NewZoneKey("Collection000000", "a")
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprint(paged), func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &cancelDuringScan{Context: base, cancel: cancel}
			var rows []Snapshot
			var err error
			if paged {
				rows, _, err = store.CausalInventoryPage(ctx, &zone, InventoryPageRequest{})
			} else {
				rows, err = store.InventoryContext(ctx, false)
			}
			if !errors.Is(err, context.Canceled) || rows != nil {
				t.Fatalf("scan returned partial success: rows=%d err=%v", len(rows), err)
			}
		})
	}
}

func TestInventoryPageServesReadsDuringTransfers(t *testing.T) {
	t.Run("frozen sibling", func(t *testing.T) {
		store, genesis := routingStore(t)
		record := Record{Collection: "Collection000000", Location: "a", ID: "row"}
		if _, _, _, err := store.ApplyRecord("seed", record); err != nil {
			t.Fatal(err)
		}
		receiver, err := Open(filepath.Join(t.TempDir(), "receiver"), "J", "test", 1<<26)
		if err != nil {
			t.Fatal(err)
		}
		defer receiver.Close()
		state, _ := store.CausalState()
		if err := receiver.InitCausalReceiver(state.Root, genesis); err != nil {
			t.Fatal(err)
		}
		op := ticketAdmission(t, store, receiver, genesis.Right().ID, tree.Region{})
		zone, _ := recordZone(record)
		if _, err := store.FreezeZones(op.Cession.ID, []tree.ZoneKey{zone}); err != nil {
			t.Fatal(err)
		}
		absent, _ := tree.NewZoneKey("Collection000000", "z")
		if rows, _, err := store.CausalInventoryPage(context.Background(), &absent, InventoryPageRequest{}); err != nil || rows != nil {
			t.Fatalf("a frozen sibling refused or filled a read: %v %v", rows, err)
		}
	})
	t.Run("uninstalled empty shard", func(t *testing.T) {
		source, genesis := routingStore(t)
		target, err := Open(filepath.Join(t.TempDir(), "target.db"), "J", "test", 1<<26)
		if err != nil {
			t.Fatal(err)
		}
		defer target.Close()
		state, _ := source.CausalState()
		if err = target.InitCausalReceiver(state.Root, genesis); err != nil {
			t.Fatal(err)
		}
		op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
		if _, err := source.ForceFreezeRegion(op.Cession.ID); err != nil {
			t.Fatal(err)
		}
		offer, err := source.SealCausalRegion(op.Cession.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = target.StageCausal(offer); err != nil {
			t.Fatal(err)
		}
		if _, err := source.EmitCausal(op.Cession.ID); err != nil {
			t.Fatal(err)
		}
		absent, _ := tree.NewZoneKey("Collection000000", "z")
		// A staged but uninstalled receiver has no coverage. Its empty answer
		// can only refer the caller to retained source data.
		if rows, _, err := target.CausalInventoryPage(context.Background(), &absent, InventoryPageRequest{}); (err != nil && !errors.Is(err, ErrNotReady)) || rows != nil {
			t.Fatalf("an uninstalled region refused or filled a read: %v %v", rows, err)
		}
	})
}

func TestMetricsProjectionUsesProjectedPageBudgetAndPreservesFullContent(t *testing.T) {
	s, _ := routingStore(t)
	for i := 0; i < 12; i++ {
		_, _, _, err := s.ApplyRecord(fmt.Sprint(i), Record{Collection: "Demo000000000000", Location: "aa", ID: fmt.Sprint(i), Metrics: []float64{float64(i)}, Payload: json.RawMessage(`{"text":"` + string(bytes.Repeat([]byte("x"), 8192)) + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
	}
	z, _ := tree.NewZoneKey("Demo000000000000", "a")
	full, page, err := s.CausalInventoryPage(context.Background(), &z, InventoryPageRequest{ByteLimit: 4096})
	if err != nil || page.Done || len(full) != 1 || len(full[0].Records[0].Payload) == 0 {
		t.Fatalf("full content changed: %v %v", page, err)
	}
	projected, page, err := s.CausalInventoryPage(context.Background(), &z, InventoryPageRequest{ByteLimit: 4096, MetricsOnly: true})
	if err != nil || !page.Done || len(projected) != 1 || len(projected[0].Records) != 12 {
		t.Fatalf("projected budget: %v %v", page, err)
	}
	for _, r := range projected[0].Records {
		if len(r.Payload) != 0 || r.Collection != "Demo000000000000" || r.Location != "aa" || len(r.Metrics) != 1 {
			t.Fatal(r)
		}
	}
}
