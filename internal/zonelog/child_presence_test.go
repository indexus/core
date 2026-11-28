package zonelog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

func TestObserveChildPresenceOverwritesAndAmbiguity(t *testing.T) {
	s, err := Open(t.TempDir(), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	apply := func(rs ...Record) {
		t.Helper()
		if err := s.Apply(rs); err != nil {
			t.Fatal(err)
		}
	}
	a := Record{Collection: "c", Location: "a0", ID: "a"}
	b := Record{Collection: "c", Location: "a0", ID: "b"}
	root := Record{Collection: "c", Location: "a", ID: "self"}
	last := Record{Collection: "c", Location: "a_012345", ID: "same"}
	apply(a, b, root, last)
	check := func(sn *Snapshot, loc string, seen, unknown uint64) {
		t.Helper()
		g, u, e := sn.observeChildPresence(context.Background(), "c", loc, false)
		if g != seen || u != unknown || e != nil {
			t.Fatalf("%s: got %x/%x, %v; want %x/%x", loc, g, u, e, seen, unknown)
		}
	}
	const high = uint64(1) << 63
	for _, disk := range []bool{false, true} {
		if disk {
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		sn := s.Snapshot()
		check(sn, "a", 1|high, 0)
		check(sn, "a0", 0, 0)
		check(sn, "@", uint64(1)<<charIndex['a'], 0)
		check(sn, strings.Repeat("a", 16), 0, 0)
		sn.Close()
	}
	// Deleting the first key leaves the other document live. The fixed
	// probe reports unknown rather than walking the other descendants.
	before := s.Snapshot()
	defer before.Close()
	a.Uncounted = true
	apply(a)
	for _, disk := range []bool{false, true} {
		if disk {
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		sn := s.Snapshot()
		check(sn, "a", high, 1)
		sn.Close()
	}
	// A newer physical key in the snapshot's mutable memtable may obscure
	// its old live record. Either witness or unknown is safe, even after flush.
	g, u, e := before.observeChildPresence(context.Background(), "c", "a", false)
	if e != nil || g&high == 0 || (g|u)&1 == 0 || g&u != 0 {
		t.Fatal(g, u, e)
	}
	a.Uncounted = false
	apply(a)
	sn := s.Snapshot()
	defer sn.Close()
	check(sn, "a", high|1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g, u, e := s.ObserveChildPresence(ctx, "c", "a"); g != 0 || u != 0 || !errors.Is(e, context.Canceled) {
		t.Fatal(g, u, e)
	}
}

type presenceIO struct {
	FS
	reads, bytes, values atomic.Int64
}
type presenceFile struct {
	File
	fs    *presenceIO
	value bool
}

func (f presenceFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	f.fs.reads.Add(1)
	f.fs.bytes.Add(int64(n))
	if f.value {
		f.fs.values.Add(int64(n))
	}
	return n, err
}
func (f *presenceIO) Open(name string) (File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return presenceFile{file, f, strings.HasSuffix(name, ".val")}, nil
}
func presenceFixture(t testing.TB, n int, flushed bool) (*Store, *presenceIO) {
	t.Helper()
	dir := t.TempDir()
	io := &presenceIO{FS: Dir(dir)}
	s, err := Open(dir, Options{NoSync: true, FS: io, MemoryBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	rs := make([]Record, n)
	for i := range rs {
		rs[i] = Record{Collection: "c", Location: "a001", ID: fmt.Sprintf("%06d", i), Metrics: []float64{42}, Payload: []byte("opaque body")}
	}
	if err := s.Apply(rs); err != nil {
		t.Fatal(err)
	}
	if flushed {
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	// Evict the page cache so the measurement starts with cold references.
	s.cache.mu.Lock()
	clear(s.cache.byKey)
	s.cache.order.Init()
	s.cache.used = 0
	s.cache.mu.Unlock()
	io.reads.Store(0)
	io.bytes.Store(0)
	io.values.Store(0)
	return s, io
}
func TestObserveChildPresenceColdReadWork(t *testing.T) {
	for _, flushed := range []bool{false, true} {
		for _, n := range []int{1, 512} {
			t.Run(fmt.Sprintf("disk=%v/rows=%d", flushed, n), func(t *testing.T) {
				s, io := presenceFixture(t, n, flushed)
				seen, unknown, err := s.ObserveChildPresence(context.Background(), "c", "a")
				if err != nil || seen != 1 || unknown != 0 {
					t.Fatal(seen, unknown, err)
				}
				calls, bytes, values := io.reads.Load(), io.bytes.Load(), io.values.Load()
				if calls > 4 || (flushed && calls == 0) || (!flushed && calls != 0) || values != 0 {
					t.Fatalf("unexpected IO %d reads, %d bytes, %d value bytes", calls, bytes, values)
				}
				t.Logf("child observation: %d reads, %d bytes, %d value bytes", calls, bytes, values)
			})
		}
	}
}

func BenchmarkObserveChildPresence(b *testing.B) {
	for _, flushed := range []bool{false, true} {
		for _, n := range []int{1, 512} {
			b.Run(fmt.Sprintf("disk=%v/rows=%d", flushed, n), func(b *testing.B) {
				s, _ := presenceFixture(b, n, flushed)
				b.ResetTimer()
				for b.Loop() {
					seen, u, err := s.ObserveChildPresence(context.Background(), "c", "a")
					if seen != 1 || u != 0 || err != nil {
						b.Fatal(seen, u, err)
					}
				}
			})
		}
	}
}

func TestObserveChildPresenceCapsSegmentReferences(t *testing.T) {
	s, _ := presenceFixture(t, 1, true)
	// The real segment with child zero sits beyond the per-request budget.
	// Empty segment headers stand in for a backlog of newer disjoint runs.
	s.mu.Lock()
	original := s.segs
	s.segs = append([]*segment{}, original...)
	for range childSegmentLimit {
		s.segs = append(s.segs, &segment{})
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.segs = original; s.mu.Unlock() }()
	sn, omitted := s.pinSnapshot(childSegmentLimit)
	size := len(sn.segs)
	sn.Close()
	if !omitted || size != childSegmentLimit {
		t.Fatal(size, omitted)
	}
	seen, unknown, err := s.ObserveChildPresence(context.Background(), "c", "a")
	if seen != 0 || unknown != ^uint64(0) || err != nil {
		t.Fatal(seen, unknown, err)
	}
}
