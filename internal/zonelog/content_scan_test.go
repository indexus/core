package zonelog

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestContentScanBudgetAndLatestVersion(t *testing.T) {
	s, io := presenceFixture(t, 12, true)
	sn, ok := s.ContentSnapshot()
	if !ok {
		t.Fatal("refused")
	}
	defer sn.Close()
	scan := func(steps, bytes int) (int, error) {
		rows := 0
		e := sn.ScanContent(context.Background(), "c", "a", steps, bytes, func(Record) error { rows++; return nil })
		return rows, e
	}
	start := io.values.Load()
	if n, e := scan(100, 1); n != 0 || !errors.Is(e, ErrContentScanLimit) || io.values.Load() != start {
		t.Fatal(n, e, io.values.Load()-start)
	}
	if n, e := scan(3, 1<<20); n > 3 || !errors.Is(e, ErrContentScanLimit) {
		t.Fatal(n, e)
	}
	if n, e := scan(100, 1<<20); n != 12 || e != nil {
		t.Fatal(n, e)
	}
	// A bounded read of the old mutable view can decline rather than chase
	// through a newer row whose history belongs to the pinned snapshot.
	if e := s.Apply([]Record{{Collection: "c", Location: "a", ID: "newer"}}); e != nil {
		t.Fatal(e)
	}
	if _, e := scan(100, 1<<20); !errors.Is(e, ErrContentScanLimit) {
		t.Fatal(e)
	}
}

func TestContentScanCountsDeadPhysicalRows(t *testing.T) {
	s, _ := presenceFixture(t, 32, true)
	var tombs []Record
	for i := range 32 {
		tombs = append(tombs, Record{Collection: "c", Location: "a001", ID: fmt.Sprintf("%06d", i), Deleted: true})
	}
	if err := s.Apply(tombs); err != nil {
		t.Fatal(err)
	}
	sn, ok := s.ContentSnapshot()
	if !ok {
		t.Fatal("refused")
	}
	defer sn.Close()
	rows := 0
	err := sn.ScanContent(context.Background(), "c", "a", 8, 1<<20, func(Record) error { rows++; return nil })
	if !errors.Is(err, ErrContentScanLimit) || rows != 0 {
		t.Fatal(err, rows)
	}
}

func TestContentSnapshotRefusesOmittedSource(t *testing.T) {
	s, _ := presenceFixture(t, 1, true)
	s.mu.Lock()
	original := s.segs
	s.segs = append([]*segment{}, s.segs...)
	for range 32 {
		s.segs = append(s.segs, &segment{})
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.segs = original; s.mu.Unlock() }()
	if sn, ok := s.ContentSnapshot(); ok {
		sn.Close()
		t.Fatal("omitted real segment")
	}
}
