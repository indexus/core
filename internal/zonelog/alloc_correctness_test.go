package zonelog

import (
	"fmt"
	"sync"
	"testing"
)

func TestGrowingScanChunksKeepMVCCAndPrefixBoundaries(t *testing.T) {
	s, err := Open(t.TempDir(), Options{NoSync: true, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var initial, updates []Record
	for i := range 2200 {
		id := fmt.Sprintf("key-%04d", i)
		initial = append(initial, Record{Collection: "c", Location: "L0", ID: id, Metrics: []float64{1}})
		if i%17 == 0 {
			updates = append(updates, Record{Collection: "c", Location: "L0", ID: id, Deleted: true})
		} else if i%31 == 0 {
			updates = append(updates, Record{Collection: "c", Location: "L0", ID: id, Metrics: []float64{2}})
		}
	}
	// IDs sharing a raw prefix must not disappear at a resumed key boundary.
	for _, id := range []string{"key-0031\x00", "key-0031\x00next"} {
		initial = append(initial, Record{Collection: "c", Location: "L0", ID: id, Metrics: []float64{1}})
	}
	initial = append(initial, Record{Collection: "c", Location: "L1", ID: "outside", Metrics: []float64{9}})
	if err := s.Apply(initial); err != nil {
		t.Fatal(err)
	}
	old := s.Snapshot()
	defer old.Close()
	if err := s.Apply(updates); err != nil {
		t.Fatal(err)
	}
	current := s.Snapshot()
	defer current.Close()
	for _, test := range []struct {
		name string
		sn   *Snapshot
		new  bool
	}{{"old", old, false}, {"current", current, true}} {
		t.Run(test.name, func(t *testing.T) {
			expected := map[string]float64{}
			for _, rec := range initial {
				if rec.Location == "L0" {
					expected[rec.ID] = 1
				}
			}
			if test.new {
				for _, rec := range updates {
					if rec.Deleted {
						delete(expected, rec.ID)
					} else {
						expected[rec.ID] = 2
					}
				}
			}
			seen := map[string]bool{}
			if err := test.sn.ScanWith("c", "L0", nil, nil, func(Record) bool { return false }, func(rec Record, _ int) bool {
				want, ok := expected[rec.ID]
				if rec.Location != "L0" || !ok || seen[rec.ID] || len(rec.Metrics) != 1 || rec.Metrics[0] != want {
					t.Fatalf("wrong/duplicate MVCC row: %+v want=%v exists=%v", rec, want, ok)
				}
				seen[rec.ID] = true
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if len(seen) != len(expected) {
				t.Fatalf("scan omitted rows: got %d want %d", len(seen), len(expected))
			}
			// A paginated continuation near the first chunk must neither repeat
			// its cursor nor lose a key whose ID extends that cursor.
			after := &Record{Location: "L0", ID: "key-0031"}
			remaining := 0
			for id := range expected {
				if id > after.ID {
					remaining++
				}
			}
			count := 0
			if err := test.sn.ScanWith("c", "L0", after, nil, func(Record) bool { return false }, func(rec Record, _ int) bool {
				if rec.ID <= after.ID || rec.Location != "L0" {
					t.Fatalf("cursor or prefix crossed: %+v", rec)
				}
				count++
				return true
			}); err != nil || count != remaining {
				t.Fatalf("continuation got %d want %d: %v", count, remaining, err)
			}
		})
	}
}

func TestPooledChildrenResultsRemainDetachedAcrossCalls(t *testing.T) {
	s, err := Open(t.TempDir(), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Apply([]Record{
		{Collection: "c", Location: "L0", ID: "a", Metrics: []float64{7, 3}},
		{Collection: "c", Location: "L1", ID: "b", Metrics: []float64{11}},
	}); err != nil {
		t.Fatal(err)
	}
	retained, err := s.Children("c", "L")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				got, err := s.Children("c", "L")
				if err != nil || got[1].Count != 1 || len(got[1].Metrics) != 2 || got[1].Metrics[0] != 7 || got[1].Only != "L0" {
					t.Errorf("reused scratch changed result: %+v %v", got[1], err)
					return
				}
				got[1].Metrics[0] = -1 // caller owns its answer, not pooled memory
			}
		}()
	}
	wg.Wait()
	if retained[1].Metrics[0] != 7 || retained[1].Metrics[1] != 3 || retained[2].Metrics[0] != 11 || retained[1].Only != "L0" {
		t.Fatal("later pooled calls modified a retained answer", retained)
	}
}
