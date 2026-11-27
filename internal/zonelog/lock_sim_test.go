package zonelog

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// slowReadFS wraps an OS/mem FS and injects a read latency on segment files
// (simulating an NVMe page read under load) without slowing WAL syncs.
type slowReadFS struct {
	FS
	readDelay atomic.Int64
	reads     atomic.Int64
}

type slowReadFile struct {
	File
	delay *atomic.Int64
	reads *atomic.Int64
}

func (f *slowReadFile) ReadAt(p []byte, off int64) (int, error) {
	f.reads.Add(1)
	if d := time.Duration(f.delay.Load()); d > 0 {
		time.Sleep(d)
	}
	return f.File.ReadAt(p, off)
}

func (fs *slowReadFS) Open(name string) (File, error) {
	f, err := fs.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return &slowReadFile{File: f, delay: &fs.readDelay, reads: &fs.reads}, nil
}

// Sim 1: Overlapping snapshots under continuous load — proves whether
// retiredSegs are reclaimed progressively or accumulate unboundedly.
func TestSimOverlappingSnapshotsRetiredSegs(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{NoSync: true, MemoryBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Always keep at least one snapshot open (rolling overlap, like continuous
	// s.view() traffic under load).
	cur := s.Snapshot()
	for round := range 25 {
		pad := make([]byte, 512)
		pad[0] = byte(round)
		if err := s.Apply([]Record{
			{Collection: "c", Location: "aa", ID: fmt.Sprintf("k%03d", round), Metrics: []float64{1}, Payload: pad},
			{Collection: "c", Location: "ab", ID: fmt.Sprintf("k%03d", round), Metrics: []float64{1}, Payload: pad},
		}); err != nil {
			t.Fatal(err)
		}
		// Wait for any background flush/merge to settle.
		s.work.Wait()
		next := s.Snapshot()
		cur.Close()
		cur = next
	}

	s.snapMu.Lock()
	retiredWhileActive := len(s.retiredSegs)
	activeSnaps := len(s.snapshots)
	s.snapMu.Unlock()
	cur.Close()
	s.snapMu.Lock()
	retiredAfterDrain := len(s.retiredSegs)
	s.snapMu.Unlock()

	t.Logf("rolling snapshots: active=%d retiredWhileActive=%d retiredAfterDrain=%d",
		activeSnaps, retiredWhileActive, retiredAfterDrain)
	if retiredWhileActive > 4 {
		t.Fatalf("retiredSegs leaked under rolling snapshots: got %d, want <= 4", retiredWhileActive)
	}
	if retiredAfterDrain != 0 {
		t.Fatalf("retiredSegs after closing all snapshots = %d, want 0", retiredAfterDrain)
	}
}

// Sim 2: Children() doing a cold segment read while Apply() commits — measures
// how long Apply() stalls waiting on s.mu.Lock() behind Children()'s disk I/O.
func TestSimChildrenDiskIOBlocksApply(t *testing.T) {
	dir := t.TempDir()
	sfs := &slowReadFS{FS: Dir(dir)}
	s, err := Open(dir, Options{FS: sfs, NoSync: true, MemoryBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Seed enough rows to flush to disk segments.
	for i := range 30 {
		pad := make([]byte, 512)
		if err := s.Apply([]Record{
			{Collection: "c", Location: "aa", ID: fmt.Sprintf("k%03d", i), Metrics: []float64{1}, Payload: pad},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// Clear page cache and inject 15 ms per disk read.
	s.cache.mu.Lock()
	clear(s.cache.byKey)
	s.cache.order.Init()
	s.cache.used = 0
	s.cache.mu.Unlock()
	sfs.readDelay.Store(int64(15 * time.Millisecond))

	done := make(chan error, 1)
	go func() {
		_, err := s.Children("c", "@")
		done <- err
	}()
	time.Sleep(5 * time.Millisecond)

	t0 := time.Now()
	if err := s.Apply([]Record{
		{Collection: "c", Location: "ab", ID: "live", Metrics: []float64{1}, Payload: []byte("x")},
	}); err != nil {
		t.Fatal(err)
	}
	applyDur := time.Since(t0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	t.Logf("Apply duration during cold Children() (15ms/read, reads=%d): %v", sfs.reads.Load(), applyDur.Round(time.Millisecond))
	if applyDur > 10*time.Millisecond {
		t.Fatalf("Apply stalled %v behind cold Children() disk I/O; expected < 10ms", applyDur)
	}
}

// Sim 3: Cold Snapshot.ScanWith() and Snapshot.Get() (including payload reads)
// must never block concurrent Apply() commits on s.mu.
func TestSimScanWithAndGetDiskIODoNotBlockApply(t *testing.T) {
	dir := t.TempDir()
	sfs := &slowReadFS{FS: Dir(dir)}
	s, err := Open(dir, Options{FS: sfs, NoSync: true, MemoryBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := range 30 {
		pad := make([]byte, 512)
		pad[0] = byte(i)
		if err := s.Apply([]Record{
			{Collection: "c", Location: "aa", ID: fmt.Sprintf("k%03d", i), Metrics: []float64{1}, Payload: pad},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.cache.mu.Lock()
	clear(s.cache.byKey)
	s.cache.order.Init()
	s.cache.used = 0
	s.cache.mu.Unlock()
	sfs.readDelay.Store(int64(15 * time.Millisecond))

	// Cold ScanWith with payloads running concurrently with Apply.
	scanDone := make(chan error, 1)
	go func() {
		sn := s.Snapshot()
		defer sn.Close()
		scanDone <- sn.ScanWith("c", "@", nil, nil, func(Record) bool { return true }, func(Record, int) bool { return true })
	}()
	time.Sleep(5 * time.Millisecond)

	t0 := time.Now()
	if err := s.Apply([]Record{
		{Collection: "c", Location: "ab", ID: "live-scan", Metrics: []float64{1}, Payload: []byte("x")},
	}); err != nil {
		t.Fatal(err)
	}
	applyDuringScan := time.Since(t0)
	if err := <-scanDone; err != nil {
		t.Fatal(err)
	}

	// Cold Get with payload running concurrently with Apply.
	s.cache.mu.Lock()
	clear(s.cache.byKey)
	s.cache.order.Init()
	s.cache.used = 0
	s.cache.mu.Unlock()
	getDone := make(chan error, 1)
	go func() {
		sn := s.Snapshot()
		defer sn.Close()
		_, ok, err := sn.Get("c", "aa", "k010")
		if err == nil && !ok {
			err = fmt.Errorf("missing k010")
		}
		getDone <- err
	}()
	time.Sleep(5 * time.Millisecond)

	t1 := time.Now()
	if err := s.Apply([]Record{
		{Collection: "c", Location: "ab", ID: "live-get", Metrics: []float64{1}, Payload: []byte("y")},
	}); err != nil {
		t.Fatal(err)
	}
	applyDuringGet := time.Since(t1)
	if err := <-getDone; err != nil {
		t.Fatal(err)
	}

	t.Logf("Apply during cold ScanWith=%v, during cold Get=%v (15ms/read)",
		applyDuringScan.Round(time.Millisecond), applyDuringGet.Round(time.Millisecond))
	if applyDuringScan > 10*time.Millisecond || applyDuringGet > 10*time.Millisecond {
		t.Fatalf("Apply stalled behind cold read I/O: scan=%v get=%v", applyDuringScan, applyDuringGet)
	}
}
