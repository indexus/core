package zonelog

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSnapshotPointInTimeIsolation(t *testing.T) {
	dir := t.TempDir()
	// Small MemoryBytes so repeated Apply calls trigger flushes and merges
	// while snapshots are held open.
	s, err := Open(dir, Options{NoSync: true, MemoryBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Batch 1: collections "a" and "b".
	if err := s.Apply([]Record{
		{Collection: "a", Location: "aa", ID: "k1", Payload: []byte("v1")},
		{Collection: "a", Location: "aa", ID: "k2", Payload: []byte("v1")},
		{Collection: "b", Location: "ab", ID: "k1", Payload: []byte("b1")},
	}); err != nil {
		t.Fatal(err)
	}
	snap1 := s.Snapshot()
	defer snap1.Close()

	// Batch 2: overwrite a/k1, delete a/k2, add a/k3 and new collection "c".
	if err := s.Apply([]Record{
		{Collection: "a", Location: "aa", ID: "k1", Payload: []byte("v2")},
		{Collection: "a", Location: "aa", ID: "k2", Deleted: true},
		{Collection: "a", Location: "aa", ID: "k3", Payload: []byte("v2")},
		{Collection: "c", Location: "aa", ID: "k1", Payload: []byte("c2")},
	}); err != nil {
		t.Fatal(err)
	}
	snap2 := s.Snapshot()
	defer snap2.Close()

	// Force multiple memtable rotations, flushes, and merges while snap1 & snap2 are open.
	for i := range 40 {
		pad := make([]byte, 512)
		pad[0] = byte(i)
		if err := s.Apply([]Record{
			{Collection: "a", Location: "aa", ID: "k1", Payload: []byte(fmt.Sprintf("v3-%d", i))},
			{Collection: "a", Location: "aa", ID: fmt.Sprintf("bulk-%03d", i), Payload: pad},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Verify snap1 still sees exact Batch 1 state.
	if got := snap1.Collections(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("snap1 collections = %v, want [a b]", got)
	}
	checkGet(t, snap1, "a", "aa", "k1", "v1", true)
	checkGet(t, snap1, "a", "aa", "k2", "v1", true)
	checkGet(t, snap1, "a", "aa", "k3", "", false)
	checkGet(t, snap1, "c", "aa", "k1", "", false)
	if ids := scanIDs(t, snap1, "a", "@"); !slices.Equal(ids, []string{"k1", "k2"}) {
		t.Fatalf("snap1 scan a = %v, want [k1 k2]", ids)
	}

	// Verify snap2 sees exact Batch 2 state.
	if got := snap2.Collections(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("snap2 collections = %v, want [a b c]", got)
	}
	checkGet(t, snap2, "a", "aa", "k1", "v2", true)
	checkGet(t, snap2, "a", "aa", "k2", "", false)
	checkGet(t, snap2, "a", "aa", "k3", "v2", true)
	checkGet(t, snap2, "c", "aa", "k1", "c2", true)
	if ids := scanIDs(t, snap2, "a", "@"); !slices.Equal(ids, []string{"k1", "k3"}) {
		t.Fatalf("snap2 scan a = %v, want [k1 k3]", ids)
	}

	// Verify live Store sees latest state.
	checkGet(t, s, "a", "aa", "k1", "v3-39", true)

	// Close snap1; snap2 must remain valid.
	snap1.Close()
	checkGet(t, snap2, "a", "aa", "k1", "v2", true)
	if ids := scanIDs(t, snap2, "a", "@"); !slices.Equal(ids, []string{"k1", "k3"}) {
		t.Fatalf("snap2 after snap1.Close = %v, want [k1 k3]", ids)
	}

	// Close snap2; retired segments and snapshot pins must be reclaimed.
	snap2.Close()
	s.snapMu.Lock()
	snaps, retired := len(s.snapshots), len(s.retiredSegs)
	s.snapMu.Unlock()
	if snaps != 0 || retired != 0 {
		t.Fatalf("after closing all snapshots: snapshots=%d retiredSegs=%d, want 0, 0", snaps, retired)
	}
}

// TestSnapshotDoesNotStarveReadersOrWriters reproduces the high-concurrency
// pattern from exp-20261123-074536: dozens of goroutines rapidly taking and
// closing snapshots concurrently with Children() scans and Apply() commits.
func TestSnapshotDoesNotStarveReadersOrWriters(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{NoSync: true, MemoryBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := range 50 {
		if err := s.Apply([]Record{
			{Collection: "c", Location: "aa", ID: fmt.Sprintf("k%03d", i), Metrics: []float64{1}, Payload: []byte("v")},
			{Collection: "c", Location: "ab", ID: fmt.Sprintf("k%03d", i), Metrics: []float64{1}, Payload: []byte("v")},
		}); err != nil {
			t.Fatal(err)
		}
	}

	var done atomic.Bool
	var wg sync.WaitGroup
	// 32 snapshotters hammering Snapshot()/Get()/Close().
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				sn := s.Snapshot()
				_, _, err := sn.Get("c", "aa", "k000")
				sn.Close()
				if err != nil {
					t.Errorf("snapshot Get: %v", err)
					return
				}
			}
		}()
	}
	// 4 Children() scanners (summary.boundaries pattern).
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				if _, err := s.Children("c", "@"); err != nil {
					t.Errorf("Children: %v", err)
					return
				}
			}
		}()
	}
	// Writer applying 150 batches concurrently.
	for i := range 150 {
		if err := s.Apply([]Record{
			{Collection: "c", Location: "aa", ID: "k000", Metrics: []float64{float64(i)}, Payload: []byte(fmt.Sprintf("v-%d", i))},
		}); err != nil {
			t.Fatal(err)
		}
	}
	done.Store(true)
	wg.Wait()
	s.snapMu.Lock()
	snaps, retired := len(s.snapshots), len(s.retiredSegs)
	s.snapMu.Unlock()
	if snaps != 0 || retired != 0 {
		t.Fatalf("after contention test: snapshots=%d retiredSegs=%d, want 0, 0", snaps, retired)
	}
}

func TestSnapshotAtomicMultiKeyConsistency(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{NoSync: true, MemoryBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Apply([]Record{
		{Collection: "c", Location: "aa", ID: "a", Payload: []byte("0")},
		{Collection: "c", Location: "aa", ID: "b", Payload: []byte("0")},
	}); err != nil {
		t.Fatal(err)
	}

	var done atomic.Bool
	var wg sync.WaitGroup
	// 4 concurrent readers taking snapshots and verifying key "a" and key "b"
	// always reflect the same atomic Apply batch.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				sn := s.Snapshot()
				ra, oka, erra := sn.Get("c", "aa", "a")
				rb, okb, errb := sn.Get("c", "aa", "b")
				var scanA, scanB string
				errScan := sn.ScanWith("c", "@", nil, nil, func(Record) bool { return true }, func(r Record, _ int) bool {
					if r.ID == "a" {
						scanA = string(r.Payload)
					} else if r.ID == "b" {
						scanB = string(r.Payload)
					}
					return true
				})
				sn.Close()
				if erra != nil || errb != nil || errScan != nil {
					t.Errorf("snapshot read error: %v %v %v", erra, errb, errScan)
					return
				}
				if !oka || !okb || string(ra.Payload) != string(rb.Payload) {
					t.Errorf("inconsistent Get: a=%q(%v) b=%q(%v)", ra.Payload, oka, rb.Payload, okb)
					return
				}
				if scanA != scanB || scanA != string(ra.Payload) {
					t.Errorf("inconsistent ScanWith vs Get: get=%q scanA=%q scanB=%q", ra.Payload, scanA, scanB)
					return
				}
			}
		}()
	}

	for i := 1; i <= 200; i++ {
		val := []byte(fmt.Sprintf("%d", i))
		if err := s.Apply([]Record{
			{Collection: "c", Location: "aa", ID: "a", Payload: val},
			{Collection: "c", Location: "aa", ID: "b", Payload: val},
		}); err != nil {
			t.Fatal(err)
		}
	}
	done.Store(true)
	wg.Wait()
}

func checkGet(t *testing.T, r Reader, col, loc, id, want string, wantOK bool) {
	t.Helper()
	rec, ok, err := r.Get(col, loc, id)
	if err != nil {
		t.Fatalf("Get(%s,%s,%s): %v", col, loc, id, err)
	}
	if ok != wantOK {
		t.Fatalf("Get(%s,%s,%s) ok=%v, want %v", col, loc, id, ok, wantOK)
	}
	if wantOK && string(rec.Payload) != want {
		t.Fatalf("Get(%s,%s,%s) = %q, want %q", col, loc, id, rec.Payload, want)
	}
}

func scanIDs(t *testing.T, r Reader, col, zone string) []string {
	t.Helper()
	var ids []string
	if err := r.ScanWith(col, zone, nil, nil, func(Record) bool { return false }, func(rec Record, _ int) bool {
		ids = append(ids, rec.ID)
		return true
	}); err != nil {
		t.Fatalf("ScanWith(%s,%s): %v", col, zone, err)
	}
	return ids
}
