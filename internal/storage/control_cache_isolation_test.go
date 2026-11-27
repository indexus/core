package storage

import "testing"

func TestControlCachesAreOwnedByEachDisk(t *testing.T) {
	var stores []*Store
	for i := 0; i < 16; i++ {
		store, _ := routingStore(t)
		if _, err := store.CausalState(); err != nil {
			t.Fatal(err)
		}
		stores = append(stores, store)
	}
	// Other disks must not evict this disk's decoded committed control. An
	// accidental process-global two-entry cache necessarily misses this pass.
	for _, store := range stores {
		if err := store.view(func(tx *Tx) error {
			tx.reader.Reader = keysOnlyReader{tx.reader.Reader, "unscanned", map[string]bool{meta(causalKey): true}}
			_, err := readCausal(tx)
			return err
		}); err != nil {
			t.Fatal("another disk evicted local control", err)
		}
	}
}
