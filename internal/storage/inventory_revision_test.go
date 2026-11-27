package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestInventoryPagesIgnoreMaintenanceAndOtherCollections(t *testing.T) {
	s, _ := routingStore(t)
	const collection = "Collection000000"
	for i := range 4 {
		if _, _, _, err := s.ApplyRecord(fmt.Sprint(i), Record{Collection: collection, Location: "a", ID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	query, _ := tree.NewZoneKey(collection, "@")
	var cursor *InventoryCursor
	seen := map[string]bool{}
	for {
		rows, page, err := s.CausalInventoryPage(context.Background(), &query, InventoryPageRequest{After: cursor, ByteLimit: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, snap := range rows {
			for _, row := range snap.Records {
				if seen[row.ID] {
					t.Fatal("duplicate row", row.ID)
				}
				seen[row.ID] = true
			}
		}
		if page.Done {
			break
		}
		cursor = page.Next
		if err := s.update(func(tx *Tx) error { return tx.put(meta([]byte("maintenance")), []byte(fmt.Sprint(len(seen)))) }); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.ApplyRecord(fmt.Sprint("other", len(seen)), Record{Collection: "Other00000000000", Location: "b", ID: fmt.Sprint(len(seen))}); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 4 {
		t.Fatal("incomplete inventory", seen)
	}
}

func TestInventoryCursorRejectsAuthorityChangeAndSurvivesReopen(t *testing.T) {
	s, g := routingStore(t)
	const collection = "Collection000000"
	for i := range 3 {
		if _, _, _, err := s.ApplyRecord(fmt.Sprint(i), Record{Collection: collection, Location: "a", ID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	query, _ := tree.NewZoneKey(collection, "@")
	_, page, err := s.CausalInventoryPage(context.Background(), &query, InventoryPageRequest{ByteLimit: 1})
	if err != nil || page.Next == nil {
		t.Fatal(page, err)
	}
	// A cursor is durable data/version information, not a retained process lease.
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, s.node, s.cluster, 1<<26)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s = reopened
	if _, _, err := s.CausalInventoryPage(context.Background(), &query, InventoryPageRequest{After: page.Next, ByteLimit: 1}); err != nil {
		t.Fatal("unchanged cursor after reopen", err)
	}
	target, err := Open(filepath.Join(t.TempDir(), "target"), "J", "test", 1<<26)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	state, _ := s.CausalState()
	if err := target.InitCausalReceiver(state.Root, g); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, s, target, g.Right().ID, tree.Region{})
	zone, _ := tree.NewZoneKey(collection, "a")
	if _, err := s.FreezeZones(op.Cession.ID, []tree.ZoneKey{zone}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EmitZoneTransfers(op.Cession.ID, []tree.ZoneKey{zone}); err != nil {
		t.Fatal(err)
	}
	if rows, _, err := s.CausalInventoryPage(context.Background(), &query, InventoryPageRequest{After: page.Next}); !errors.Is(err, ErrReadChanged) || rows != nil {
		t.Fatal("changed authority accepted", err)
	}
}

func TestBulkCutInvalidatesCollectionCursorAtomically(t *testing.T) {
	s, _ := bulkStore(t, filepath.Join(t.TempDir(), "store"))
	defer s.Close()
	for i := range 2 {
		if _, _, _, err := s.ApplyRecord(fmt.Sprint(i), Record{Collection: bulkCollection, Location: "a", ID: fmt.Sprint(i), Metrics: []float64{1, 2}}); err != nil {
			t.Fatal(err)
		}
	}
	query, _ := tree.NewZoneKey(bulkCollection, "@")
	for round := range 2 {
		_, page, err := s.CausalInventoryPage(context.Background(), &query, InventoryPageRequest{ByteLimit: 1})
		if err != nil || page.Next == nil {
			t.Fatal(page, err)
		}
		dir := filepath.Join(t.TempDir(), "batch")
		writeBatchDir(t, dir, bulkRows("b", 3, round+1))
		if _, err := s.IngestBulk(context.Background(), dir, true); err != nil {
			t.Fatal(err)
		}
		if rows, _, err := s.CausalInventoryPage(context.Background(), &query, InventoryPageRequest{After: page.Next}); !errors.Is(err, ErrReadChanged) || rows != nil {
			t.Fatal("bulk changed content without invalidating cursor", err)
		}
	}
}

// A collection stamp uses the serialized data cut. Updating it needs no
// point lookup, and unrelated commits need not touch the stamp.
func TestInventoryRevisionUsesCutSequenceWithoutLookup(t *testing.T) {
	tx := &Tx{writable: true, version: 41, reader: &txReader{}}
	if err := tx.advanceInventory(map[string]bool{"A": true, "B": true}); err != nil {
		t.Fatal(err)
	}
	for _, collection := range []string{"A", "B"} {
		payload := tx.pending[inventoryRevisionPath(collection)]
		if len(payload) != 9 || binary.BigEndian.Uint64(payload[1:]) != 42 {
			t.Fatalf("wrong cut stamp %x", payload)
		}
	}
	overflow := &Tx{writable: true, version: math.MaxUint64 - 1, reader: &txReader{}}
	if err := overflow.advanceInventory(map[string]bool{"A": true}); !errors.Is(err, ErrSequence) {
		t.Fatal("stamp wrapped", err)
	}
}
