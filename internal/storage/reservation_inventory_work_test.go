package storage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// An empty session inventory is still a completed lookup. Growing an
// ordinary ledger-only batch must not repeat that lookup per request.
func TestLedgerOnlyReservationLoadsSessionInventoryAtMostOnce(t *testing.T) {
	for _, requests := range []int{1, 16, 64} {
		t.Run(fmt.Sprint(requests), func(t *testing.T) {
			store, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger"), "D", "test", 1<<24, Options{NoSync: true})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.update(func(tx *Tx) error {
				var paths []string
				for i := range requests {
					r := RequestReceipt{RequestID: fmt.Sprint("r-", i), Collection: "Demo000000000000", Location: "a", Seq: uint64(i + 1)}
					raw, err := json.Marshal(r)
					if err != nil {
						return err
					}
					path := key(operationsBucket, []byte(r.RequestID))
					if err := tx.put(path, raw); err != nil {
						return err
					}
					paths = append(paths, path)
				}
				before := reservationInventories.Load()
				delta, err := reservationDelta(tx, paths)
				if calls := reservationInventories.Load() - before; calls > 1 {
					t.Errorf("%d requests loaded an empty session inventory %d times", requests, calls)
				}
				if delta != 0 {
					t.Errorf("ledger-only writes reserved %d bytes", delta)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			assertStoredUsage(t, store)
		})
	}
}
