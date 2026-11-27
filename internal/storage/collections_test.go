package storage

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/algebra"
)

// describedCollection is a base-64 collection, 16 characters, two axes,
// records at depths 2 to 16, columns [sum, max, last].
func describedCollection(t *testing.T, nonce byte) (string, []byte) {
	t.Helper()
	d := &algebra.Descriptor{
		BitsPerChar:  6,
		Width:        16,
		Nonce:        [16]byte{nonce},
		Space:        algebra.Space{Axes: []algebra.Axis{{Min: 0, Max: 1 << 20}, {Min: 0, Max: 1 << 20}}, Schedule: algebra.RoundRobin(2, 96)},
		RecordDepths: [2]int{2, 16},
		Schema:       algebra.Schema{algebra.Sum, algebra.Max, algebra.Last},
	}
	b, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	name, _ := d.Name()
	return name, b
}

func TestCatalogAdmitsVerifiesAndSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	store := openTestData(t, path, "D")
	name, b := describedCollection(t, 1)
	other, _ := describedCollection(t, 2)

	if c, err := store.CollectionOf(name); err != nil || c != nil {
		t.Fatalf("an unknown collection reads as legacy: %v %v", c, err)
	}
	for i := 0; i < 2; i++ {
		c, err := store.AdmitCollection(name, b)
		if err != nil {
			t.Fatal(err)
		}
		if c.Name != name || c.Digest != algebra.Digest(b) || !c.Typed() {
			t.Fatalf("admitted %+v", c)
		}
	}
	if _, err := store.AdmitCollection(other, b); !errors.Is(err, algebra.ErrName) {
		t.Fatalf("a description admitted under another name: %v", err)
	}
	tampered := append([]byte(nil), b...)
	tampered[len(tampered)-1] ^= 1
	if _, err := store.AdmitCollection(name, tampered); err == nil {
		t.Fatalf("tampered bytes admitted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestData(t, path, "D")
	defer store.Close()
	c, err := store.CollectionOf(name)
	if err != nil || c == nil || c.Digest != algebra.Digest(b) {
		t.Fatalf("description lost on restart: %v %v", c, err)
	}
}

func TestCatalogRefusesASecondDescriptionForAHeldName(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "collide.db"), "D")
	defer store.Close()
	name, b := describedCollection(t, 1)
	_, forged := describedCollection(t, 3)
	// A forged collision: other bytes already held under this name.
	if err := store.update(func(tx *Tx) error { return tx.put(key(collectionsBucket, []byte(name)), forged) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitCollection(name, b); !errors.Is(err, algebra.ErrCollision) {
		t.Fatalf("a second description for one name: %v", err)
	}
}

func TestWritesFollowTheDescription(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "fit.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	name, b := describedCollection(t, 1)

	// Before admission the collection is legacy: any width goes, and the
	// legacy mark is cached.
	if _, err := store.Write("legacy", Record{Collection: name, Location: "aa", ID: "x", Metrics: []float64{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitCollection(name, b); err != nil {
		t.Fatal(err)
	}
	for what, r := range map[string]Record{
		"width": {Collection: name, Location: "aa", ID: "w", Metrics: []float64{1, 2}},
		"depth": {Collection: name, Location: "a", ID: "d", Metrics: []float64{1, 2, 3}},
	} {
		if _, err := store.Write(what, r); !errors.Is(err, ErrDescription) {
			t.Fatalf("%s: %v", what, err)
		}
	}
	if _, err := store.Write("fits", Record{Collection: name, Location: "aab", ID: "f", Metrics: []float64{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write("delete", Record{Collection: name, Location: "aa", ID: "x", Deleted: true}); err != nil {
		t.Fatalf("a delete carries no metrics: %v", err)
	}
	if _, err := store.Write("legacy-other", Record{Collection: "Demo000000000000", Location: "aa", ID: "y", Metrics: []float64{1, 2, 3, 4}}); err != nil {
		t.Fatal(err)
	}
}

func describedWith(t *testing.T, schema algebra.Schema) (string, []byte) {
	t.Helper()
	d := &algebra.Descriptor{BitsPerChar: 6, Width: 16, RecordDepths: [2]int{1, 16}, Schema: schema}
	b, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	name, _ := d.Name()
	return name, b
}

func TestBulkBatchesFollowTheDescription(t *testing.T) {
	s, _ := bulkStore(t, filepath.Join(t.TempDir(), "bulk.db"))
	defer s.Close()
	fits, fb := describedWith(t, algebra.Schema{algebra.Sum, algebra.Max})
	wide, wb := describedWith(t, algebra.Schema{algebra.Sum, algebra.Max, algebra.Last})
	for name, b := range map[string][]byte{fits: fb, wide: wb} {
		if _, err := s.AdmitCollection(name, b); err != nil {
			t.Fatal(err)
		}
	}
	rows := bulkRows("A", 200, 3) // two metrics per row
	for name, refused := range map[string]bool{fits: false, wide: true} {
		dir := t.TempDir()
		writeBatchDirOf(t, dir, name, rows)
		_, err := ingestDir(s, dir, false)
		if refused != errors.Is(err, ErrDescription) || !refused && err != nil {
			t.Fatalf("%s: refused=%v err=%v", name, refused, err)
		}
	}
	// The typed fold reads the batch record by record.
	n := 0
	if err := s.ZoneRecords(fits, "@", func(r ZoneRecord) bool { n++; return len(r.Metrics) == 2 }); err != nil || n != len(rows) {
		t.Fatalf("%d records read back (%v)", n, err)
	}
}
