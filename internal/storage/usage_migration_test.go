package storage

import (
	"fmt"
	"path/filepath"
	"testing"
)

// A store written before data-usage-2 has the former key ("data-usage",
// which also counted bucket names) and not the new one. Opened by this
// build, its usage is a scan of what it holds, never the former value, and
// the first write stores the new key, exact from then on.
func TestUsageOfAStoreWrittenBeforeDataUsage2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	store := openTestData(t, path, "D")
	initTestCausal(t, store)
	write := func(s *Store, i int) {
		t.Helper()
		r := Record{Collection: "Collection000000", Location: fmt.Sprintf("Z%03d", i%40), ID: fmt.Sprint("r", i), Metrics: []float64{float64(i)}}
		if _, _, _, err := s.ApplyRecord(r.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 200 {
		write(store, i)
	}
	// What this store holds, counted the data-usage-2 way.
	_, want, err := store.CapacityUsage()
	if err != nil {
		t.Fatal(err)
	}
	// Make it a store of the former build: the new key gone, the former key
	// there with another value.
	if err := store.update(func(tx *Tx) error {
		if err := tx.putJSON(meta([]byte("data-usage")), map[string]int64{"Owned": want.Owned + 12345}); err != nil {
			return err
		}
		return tx.set(meta(usageKey), nil)
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store = openTestData(t, path, "D")
	defer store.Close()
	_, got, err := store.CapacityUsage()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("usage of the old store %+v, want the scan %+v", got, want)
	}
	write(store, 1000)
	var stored bool
	_ = store.view(func(tx *Tx) error { stored = tx.value(meta(usageKey)) != nil; return nil })
	if !stored {
		t.Fatal("the first write did not store data-usage-2")
	}
	var scanned, kept = want, want
	_ = store.view(func(tx *Tx) error {
		scanned, err = store.dataUsage(tx)
		if err == nil {
			kept, err = store.storedDataUsage(tx)
		}
		return err
	})
	if err != nil || kept != scanned {
		t.Fatalf("after the first write: stored %+v, scan %+v (%v)", kept, scanned, err)
	}
}
