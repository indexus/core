package storage

import (
	"fmt"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"testing"
)

// compactZoneRows reads each batch from where the last one stopped. A region
// of several batches must still lose every zone state row and every
// ZoneTransfer, none skipped at a batch boundary, and an operation not
// installed keeps them all.
func TestCompactZoneRowsDropsEveryBatch(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "compact.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	operation := causal.TransferID{Actor: "disk", Sequence: 1}
	const zones = 4*compactBatchZones + 117
	put := func(installed bool) {
		t.Helper()
		if err := store.updateCausal(func(tx *Tx, state CausalState) error {
			transfer := state.Transfers[operation]
			transfer.Cession.ID = operation
			transfer.Phase = causal.TransferMoving
			if installed {
				transfer.Phase = causal.TransferInstalled
			}
			if state.Transfers == nil {
				state.Transfers = map[causal.TransferID]CausalTransfer{}
			}
			state.Transfers[operation] = transfer
			for i := range zones {
				z, err := tree.NewZoneKey("compactrowstest0", fmt.Sprintf("Z%05d", i))
				if err != nil {
					return err
				}
				if err := putZoneState(tx, operation, ZoneState{Zone: z, Installed: true}); err != nil {
					return err
				}
				// One zone in three also has its ZoneTransfer row.
				if i%3 == 0 {
					if err := tx.set(zoneTransferPath(z, operation), []byte(`{}`)); err != nil {
						return err
					}
				}
			}
			return writeCausal(tx, state)
		}); err != nil {
			t.Fatal(err)
		}
	}
	left := func() (n int) {
		_ = store.view(func(tx *Tx) error {
			return tx.each(zoneStatesRows(operation), "", func(string, []byte) bool { n++; return true })
		})
		return n
	}

	put(false)
	if err := store.compactZoneRows(operation); err == nil {
		t.Fatal("compacted the zone rows of an operation not installed")
	}
	if n := left(); n != zones {
		t.Fatalf("%d zone rows left of %d before the install", n, zones)
	}
	put(true)
	if err := store.compactZoneRows(operation); err != nil {
		t.Fatal(err)
	}
	if n := left(); n != 0 {
		t.Fatalf("%d zone rows left after compaction", n)
	}
	_ = store.view(func(tx *Tx) error {
		for i := 0; i < zones; i += 3 {
			z, _ := tree.NewZoneKey("compactrowstest0", fmt.Sprintf("Z%05d", i))
			if p, err := tx.get(zoneTransferPath(z, operation)); err != nil || p != nil {
				t.Fatalf("ZoneTransfer of %s left after compaction (%v)", z.Location, err)
			}
		}
		return nil
	})
}
