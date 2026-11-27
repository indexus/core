package zonelog

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type prepareReadGate struct {
	FS
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

type prepareReadFile struct {
	File
	gate *prepareReadGate
}

func (f prepareReadFile) ReadAt(p []byte, at int64) (int, error) {
	if f.gate.armed.CompareAndSwap(true, false) {
		close(f.gate.entered)
		<-f.gate.release
	}
	return f.File.ReadAt(p, at)
}

func (g *prepareReadGate) Open(name string) (File, error) {
	f, err := g.FS.Open(name)
	return prepareReadFile{f, g}, err
}

func TestColdApplyPreparationDoesNotLockReadersAndRevalidatesConcurrentCommit(t *testing.T) {
	dir := t.TempDir()
	fs := &prepareReadGate{FS: Dir(dir), entered: make(chan struct{}), release: make(chan struct{})}
	s, err := Open(dir, Options{FS: fs, NoSync: true, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record := Record{Collection: "c", Location: "@", ID: "key", Metrics: []float64{1}}
	if err := s.Apply([]Record{record}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.cache.mu.Lock()
	clear(s.cache.byKey)
	s.cache.order.Init()
	s.cache.used = 0
	s.cache.mu.Unlock()
	fs.armed.Store(true)
	var once sync.Once
	release := func() { once.Do(func() { close(fs.release) }) }
	done := make(chan error, 1)
	record.Metrics = []float64{3}
	go func() { defer close(done); done <- s.Apply([]Record{record}) }()
	defer func() {
		release()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Apply did not stop")
		}
	}()
	select {
	case <-fs.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cold read was not exercised")
	}
	readable := make(chan struct{})
	go func() { s.Seq(); close(readable) }()
	select {
	case <-readable:
	case <-time.After(time.Second):
		t.Fatal("cold Apply held the engine lock across disk I/O")
	}
	// Publish and flush another version while the first preparation is pinned.
	// Omitting its seq recheck produces an incorrect aggregate retraction.
	if err := s.Apply([]Record{{Collection: "c", Location: "@", ID: "key", Metrics: []float64{7}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sums, err := s.Children("c", "@")
	if err != nil || sums[0].Count != 1 || len(sums[0].Metrics) != 1 || sums[0].Metrics[0] != 3 {
		t.Fatalf("stale preparation corrupted sums: %+v %v", sums[0], err)
	}
}
