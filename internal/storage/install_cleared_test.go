package storage

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// recordGets counts the point reads of record rows.
type recordGets struct {
	zonelog.Reader
	n int
}

func (r *recordGets) Get(collection, location, id string) (zonelog.Record, bool, error) {
	if collection != kvCollection {
		r.n++
	}
	return r.Reader.Get(collection, location, id)
}

func stagedSnapshotForTest(z tree.ZoneKey, from, to int) Snapshot {
	snap := Snapshot{Zone: z, Seq: uint64(to)}
	for i := from; i < to; i++ {
		snap.Records = append(snap.Records, Record{Collection: z.Collection, Location: z.Location, ID: fmt.Sprintf("r%03d", i), Metrics: []float64{float64(i)}})
	}
	return snap
}

func stageCertified(t *testing.T, tx *Tx, snap Snapshot) Receipt {
	t.Helper()
	if err := stageSnapshot(tx, "op", "S", snap); err != nil {
		t.Fatal(err)
	}
	if err := certifySnapshot(tx, "op", "S", snap); err != nil {
		t.Fatal(err)
	}
	seal, err := snap.Seal()
	if err != nil {
		t.Fatal(err)
	}
	return Receipt{Source: "S", Target: "D", Seal: seal}
}

// An install clears the zone, then writes the staged records over it. The
// clear already knows every committed row of the zone: the install reads no
// record row again to learn that the row it writes replaces nothing.
func TestInstallStagedZoneReadsNoRecordRowAfterTheClear(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "install.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	z, err := tree.NewZoneKey("installcleared00", "Z")
	if err != nil {
		t.Fatal(err)
	}
	reader := &recordGets{Reader: store.db}
	tx := store.transaction(reader, true)
	receipt := stageCertified(t, tx, stagedSnapshotForTest(z, 0, 50))
	reader.n = 0
	if err := store.installStagedZone(tx, "op", receipt); err != nil {
		t.Fatal(err)
	}
	if rows := tx.records[string(zoneBytes(z))]; rows == nil || len(rows.rows) != 50 {
		t.Fatalf("the install buffered %v, want 50 records", rows)
	}
	if reader.n != 0 {
		t.Fatalf("the install of 50 records over a cleared zone read %d record rows", reader.n)
	}
}

// An install over a zone that already holds rows replaces them all: the
// rows it does not carry are gone, the ones it carries have its values, and
// the usage is exact (every commit of a test compares it with a scan).
func TestInstallStagedZoneReplacesTheLiveRows(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "install.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	z, err := tree.NewZoneKey("installcleared00", "Z")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		r := Record{Collection: z.Collection, Location: z.Location, ID: fmt.Sprintf("r%03d", i), Payload: []byte(`"old value"`)}
		if _, err := store.Write(fmt.Sprint("w", i), r); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.update(func(tx *Tx) error {
		return store.installStagedZone(tx, "op", stageCertified(t, tx, stagedSnapshotForTest(z, 3, 10)))
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Snapshot(z)
	if err != nil || len(snap.Records) != 7 {
		t.Fatalf("%d live records after the install, want 7 (%v)", len(snap.Records), err)
	}
	for i, r := range snap.Records {
		if r.ID != fmt.Sprintf("r%03d", i+3) || len(r.Payload) != 0 || len(r.Metrics) != 1 {
			t.Fatalf("live record %d after the install: %+v", i, r)
		}
	}
}

// BenchmarkInstallStagedZone installs one staged zone of 2,000 records on a
// disk that holds 20,000 records of other zones in flushed segments.
func BenchmarkInstallStagedZone(b *testing.B) {
	store, err := OpenWithOptions(filepath.Join(b.TempDir(), "install.db"), "D", "test", 1<<30, Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	holder, _ := store.Endpoint("D")
	if err := store.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
		b.Fatal(err)
	}
	for i := range 20000 {
		r := Record{Collection: "installcleared00", Location: fmt.Sprintf("A%02d", i%50), ID: fmt.Sprint("o", i)}
		if _, err := store.Write(fmt.Sprint("w", i), r); err != nil {
			b.Fatal(err)
		}
		if i%5000 == 4999 {
			if err := store.db.Flush(); err != nil {
				b.Fatal(err)
			}
		}
	}
	z, _ := tree.NewZoneKey("installcleared00", "Z")
	snap := stagedSnapshotForTest(z, 0, 2000)
	seal, err := snap.Seal()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		tx := store.transaction(store.db, true)
		if err := stageSnapshot(tx, "op", "S", snap); err != nil {
			b.Fatal(err)
		}
		if err := certifyZone(tx, "op", "S", seal); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := store.installStagedZone(tx, "op", Receipt{Source: "S", Target: "D", Seal: seal}); err != nil {
			b.Fatal(err)
		}
	}
}
