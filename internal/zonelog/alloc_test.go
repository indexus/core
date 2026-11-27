package zonelog

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/indexus/go-indexus-core/internal/testkit"
)

// bytesPerRun is the heap a call allocates, averaged over runs.
func bytesPerRun(runs int, f func()) uint64 {
	f() // warm pools and caches
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

// A summary flush reads Children for every dirty parent and a scan often
// stops at its first row: neither may allocate in proportion to the 65 spans
// or to the memtable chunk. The bounds are several times the measured cost
// (about 0.6 KiB each) and well under the former one (14 KiB and 30 KiB).
func TestSumAndScanWorkingMemoryIsReused(t *testing.T) {
	if testkit.Race {
		t.Skip("the race detector makes sync.Pool drop what it is given")
	}
	s, err := Open(t.TempDir(), Options{MemoryBytes: 64 << 20, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	batch := make([]Record, 0, 4096)
	for i := range 4096 {
		batch = append(batch, Record{Collection: testCollection, Location: "L" + string(alphabet[i%64]) + string(alphabet[i/64]), ID: fmt.Sprint(i), Metrics: []float64{1}})
	}
	if err := s.Apply(batch); err != nil {
		t.Fatal(err)
	}
	children := bytesPerRun(200, func() {
		got, err := s.Children(testCollection, "L")
		if err != nil || got[1].Count != 64 {
			t.Fatalf("children: %v %+v", err, got[1])
		}
	})
	if children > 3<<10 {
		t.Fatalf("Children allocates %d bytes per call", children)
	}
	scan := bytesPerRun(200, func() {
		seen := 0
		s.Scan(testCollection, "L", nil, func(Record) bool { seen++; return false })
		if seen != 1 {
			t.Fatalf("scan visited %d rows", seen)
		}
	})
	if scan > 3<<10 {
		t.Fatalf("a scan stopped at its first row allocates %d bytes", scan)
	}
	t.Logf("Children %d B/call, early scan %d B/call", children, scan)
}
