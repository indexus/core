package storage

import (
	"fmt"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// The transaction removes one control subtree while unrelated, already
// deleted rows still occupy the following part of the same keyspace.
func prefixScanFixture(t testing.TB, flushed bool) (*zonelog.Store, string) {
	t.Helper()
	db, err := zonelog.Open(t.TempDir(), zonelog.Options{NoSync: true, MemoryBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	prefix := key([]byte("middle"))
	row := func(path string) zonelog.Record {
		return zonelog.Record{Collection: kvCollection, Location: "@", ID: path, Payload: []byte("v")}
	}
	batch := []zonelog.Record{row(prefix), row(prefix + element([]byte("child")))}
	for i := range 4096 {
		batch = append(batch, row(key([]byte("tail"), []byte(fmt.Sprintf("%06d", i)))))
	}
	if err := db.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if flushed {
		if err := db.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	for i := range batch[2:] {
		batch[i+2].Deleted = true
		batch[i+2].Payload = nil
	}
	if err := db.Apply(batch[2:]); err != nil {
		t.Fatal(err)
	}
	return db, prefix
}

func TestPrefixDropSkipsNeighborTombstones(t *testing.T) {
	for _, flushed := range []bool{false, true} {
		t.Run(fmt.Sprintf("flushed=%v", flushed), func(t *testing.T) {
			db, prefix := prefixScanFixture(t, flushed)
			tx := newTx(db, true)
			work := measureReceiver(t, func() error { return tx.drop(prefix) })
			if len(tx.pending) != 2 || work.keys != 2 || work.dead != 0 {
				t.Fatalf("drop staged %d rows, scanned %d keys including %d dead; want 2, 2, 0", len(tx.pending), work.keys, work.dead)
			}
		})
	}
}

// The first page starts at an inclusive encoded path. Later pages resume
// strictly after their cursor, including when a pending row shadows that key.
func TestPrefixPagesMergePendingRowsAtEscapedCursor(t *testing.T) {
	db, err := zonelog.Open(t.TempDir(), zonelog.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prefix := key([]byte("A\x00B"))
	child := func(i int) string { return prefix + element([]byte{0, byte(i)}) }
	var committed []zonelog.Record
	var want []string
	for i := range 29 {
		path := child(i)
		if i%2 == 0 {
			committed = append(committed, zonelog.Record{Collection: kvCollection, Location: "@", ID: path, Payload: []byte("vold")})
		}
		if i >= 6 && i != 14 {
			want = append(want, path)
		}
	}
	if err := db.Apply(committed); err != nil {
		t.Fatal(err)
	}
	tx := newTx(db, true)
	for i := 1; i < 29; i += 2 {
		if err := tx.put(child(i), []byte("new")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.put(child(6), []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err := tx.del(child(14)); err != nil {
		t.Fatal(err)
	}
	var got []string
	err = tx.rows(prefix, child(6), func(path string, payload []byte) bool {
		if path == child(6) && string(payload) != "vreplacement" {
			t.Fatalf("shadowed cursor payload %q", payload)
		}
		got = append(got, path)
		return true
	})
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func BenchmarkPrefixDropAfterDeletes(b *testing.B) {
	db, prefix := prefixScanFixture(b, true)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		tx := newTx(db, true)
		if err := tx.drop(prefix); err != nil || len(tx.pending) != 2 {
			b.Fatalf("drop: %d rows, %v", len(tx.pending), err)
		}
	}
}
