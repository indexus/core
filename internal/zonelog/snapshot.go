package zonelog

import (
	"bytes"
	"slices"
	"strings"
	"sync/atomic"
)

// Counters of the physical work of scans, for tests: a scan that returns
// eight live rows can walk any number of tombstones to find them (measured
// 2026-11-26, docs/benchmarks/receiver-writer-20261126.md). One add per scan.
var (
	scanKeys atomic.Int64 // distinct keys a ScanWith examined, live or not
	scanDead atomic.Int64 // those it stepped over: tombstones and retracted keys
)

// ScanWork is how many keys the scans of this process examined, and how
// many of them were dead (tombstones a merge has not dropped yet). For tests.
func ScanWork() (keys, dead int64) { return scanKeys.Load(), scanDead.Load() }

// IDRange restricts a scan to records at the exact location whose IDs start
// with Prefix and are at or above From in byte order. A nil range includes
// descendants. After, when supplied, remains an exclusive pagination cursor.
type IDRange struct {
	Prefix string
	From   string
}

// Reader is the read surface shared by a live Store and a point-in-time Snapshot.
type Reader interface {
	Seq() uint64
	Get(collection, location, id string) (Record, bool, error)
	ScanWith(collection, zone string, after *Record, ids *IDRange, load func(Record) bool, visit func(Record, int) bool) error
	Collections() []string
}

// Snapshot is an isolated MVCC read view of a Store at a fixed committed seq.
// Concurrent Apply/fsync commits and background flushes/merges never mutate
// what an open Snapshot sees, and reads on a Snapshot never block fsync.
// Call Close when done so replaced segments and superseded memtable versions
// can be reclaimed.
type Snapshot struct {
	s      *Store
	seq    uint64
	mem    *skiplist
	imm    *skiplist
	segs   []*segment
	cols   int // prefix length of s.colNames present at snapshot time
	closed bool
}

// Snapshot pins the current committed version of s for consistent reads.
// Uses s.mu.RLock() so concurrent snapshots, readers, and Children() scans
// run in parallel without turning reads into exclusive writers on s.mu.
func (s *Store) Snapshot() *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seq := s.visible
	s.snapMu.Lock()
	s.snapshots = append(s.snapshots, seq)
	s.snapMu.Unlock()
	return &Snapshot{
		s:    s,
		seq:  seq,
		mem:  s.mem,
		imm:  s.imm,
		segs: slices.Clone(s.segs),
		cols: len(s.colNames),
	}
}

// retiredSeg is a merged-away segment kept open until every snapshot with
// seq <= retiredAt has closed. Any snapshot opened at seq > retiredAt already
// sees the replacement segment and can never touch seg.
type retiredSeg struct {
	seg       *segment
	retiredAt uint64
}

// hasReclaimableLocked reports whether any retiredSeg can be safely closed.
// snapMu must be held.
func (s *Store) hasReclaimableLocked() bool {
	if len(s.retiredSegs) == 0 {
		return false
	}
	if len(s.snapshots) == 0 {
		return true
	}
	oldest := s.snapshots[0]
	for _, r := range s.retiredSegs {
		if r.retiredAt < oldest {
			return true
		}
	}
	return false
}

// takeReclaimableLocked removes and returns all retired segments unreachable by
// any active snapshot. snapMu must be held.
func (s *Store) takeReclaimableLocked() []*segment {
	if len(s.retiredSegs) == 0 {
		return nil
	}
	if len(s.snapshots) == 0 {
		out := make([]*segment, len(s.retiredSegs))
		for i, r := range s.retiredSegs {
			out[i] = r.seg
		}
		s.retiredSegs = nil
		return out
	}
	oldest := s.snapshots[0]
	var out []*segment
	kept := s.retiredSegs[:0]
	for _, r := range s.retiredSegs {
		if r.retiredAt < oldest {
			out = append(out, r.seg)
		} else {
			kept = append(kept, r)
		}
	}
	s.retiredSegs = kept
	return out
}

// Close releases the snapshot and reclaims any retired segments unreachable by
// older active snapshots. The common path never acquires s.mu.
func (sn *Snapshot) Close() {
	if sn == nil || sn.s == nil {
		return
	}
	s := sn.s
	s.snapMu.Lock()
	if sn.closed {
		s.snapMu.Unlock()
		return
	}
	sn.closed = true
	if i := slices.Index(s.snapshots, sn.seq); i >= 0 {
		s.snapshots = slices.Delete(s.snapshots, i, i+1)
	}
	reclaim := s.hasReclaimableLocked()
	s.snapMu.Unlock()
	if !reclaim {
		return
	}
	// Preserve lock order s.mu -> s.snapMu and re-check under both locks.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapMu.Lock()
	retired := s.takeReclaimableLocked()
	s.snapMu.Unlock()
	if len(retired) == 0 {
		return
	}
	for _, old := range retired {
		old.f.Close()
		s.fs.Remove(segName(old.id))
	}
	if err := s.dropPayloadFiles(); err != nil && s.err == nil {
		s.err = err
	}
}

func (sn *Snapshot) Seq() uint64 { return sn.seq }

// memScanChunk bounds how many rows memChunkIter copies from the mutable
// memtable per brief s.mu.RLock() hold. This keeps ScanWith O(N) even when
// scanPaged stops after 512 records on a large memtable, and bounds RLock hold
// time to microseconds.
const memScanChunk = 1024

// memScanFirst is the first chunk: most scans read one small zone or stop at
// their first live row, and must not pay for 1024 rows they never visit.
const memScanFirst = 32

// memChunkIter lazily streams visible rows from sn.mem in bounded chunks,
// releasing s.mu.RLock() between chunks so disk I/O and large scans never
// block Apply().
type memChunkIter struct {
	s      *Store
	mem    *skiplist
	seq    uint64
	lo, hi []byte
	buf    []*row
	i      int
	chunk  int // rows of the next refill: memScanFirst, doubling to memScanChunk
	done   bool
}

func newMemChunkIter(sn *Snapshot, lo, hi []byte) *memChunkIter {
	it := &memChunkIter{s: sn.s, mem: sn.mem, seq: sn.seq, lo: lo, hi: hi, chunk: memScanFirst}
	it.refill()
	return it
}

func (it *memChunkIter) refill() {
	it.buf = it.buf[:0]
	it.i = 0
	if it.done {
		return
	}
	chunk := it.chunk
	it.chunk = min(2*chunk, memScanChunk)
	if cap(it.buf) < chunk {
		it.buf = make([]*row, 0, chunk+2) // a key's last versions may follow
	}
	it.s.mu.RLock()
	defer it.s.mu.RUnlock()
	mit := it.mem.from(it.lo, it.seq)
	var lastKey []byte
	for r := mit.row(); r != nil && bytes.Compare(r.key, it.hi) < 0; r = mit.row() {
		// Stop only on a key boundary so a row and its r.old stay together.
		if len(it.buf) >= chunk && !bytes.Equal(r.key, lastKey) {
			it.lo = append(bytes.Clone(lastKey), 0)
			return
		}
		it.buf = append(it.buf, r)
		lastKey = r.key
		_ = mit.next()
	}
	it.done = true
}

func (it *memChunkIter) row() *row {
	if it.i >= len(it.buf) {
		return nil
	}
	return it.buf[it.i]
}

func (it *memChunkIter) next() error {
	it.i++
	if it.i >= len(it.buf) && !it.done {
		it.refill()
	}
	return nil
}

func (sn *Snapshot) lookup(collection, location, id string) (*row, error) {
	s := sn.s
	s.mu.RLock()
	col, ok := s.cols[collection]
	if !ok {
		s.mu.RUnlock()
		return nil, nil
	}
	key, err := makeKey(col, location, id)
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	r := sn.mem.get(key).at(sn.seq)
	s.mu.RUnlock()

	if r == nil {
		if r = sn.imm.get(key).at(sn.seq); r == nil {
			for i := len(sn.segs) - 1; i >= 0; i-- {
				if r, err = sn.segs[i].get(s.cache, key); r != nil || err != nil {
					break
				}
			}
		}
	}
	return r, err
}

func (sn *Snapshot) Get(collection, location, id string) (Record, bool, error) {
	r, err := sn.lookup(collection, location, id)
	if err != nil || r == nil || !r.live() {
		return Record{}, false, err
	}
	if err := sn.s.payload(r); err != nil {
		return Record{}, true, err
	}
	return recordWithCollection(collection, r), true, nil
}

func (sn *Snapshot) ScanWith(collection, zone string, after *Record, ids *IDRange, load func(Record) bool, visit func(Record, int) bool) error {
	s := sn.s
	s.mu.RLock()
	col, ok := s.cols[collection]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	lo, hi, err := zoneRange(col, zone)
	if err != nil {
		return err
	}
	if ids != nil {
		// The physical key is the fixed-size location head followed by raw
		// ID bytes. Even an all-0xff prefix has a successor: the depth byte
		// at the end of the head is at most 16. From can be synthetic (e.g.
		// a 255-byte ID followed by zero) and need not be a storable ID.
		hi = append(hi[:0], lo...)
		hi = append(hi, ids.Prefix...)
		i := len(hi) - 1
		for hi[i] == 0xff {
			i--
		}
		hi[i]++
		hi = hi[:i+1]
		lo = append(lo, max(ids.Prefix, ids.From)...)
	}
	if after != nil {
		key, err := makeKey(col, after.Location, after.ID)
		if err != nil {
			return err
		}
		if cursor := append(key, 0); bytes.Compare(cursor, lo) > 0 {
			lo = cursor
		}
	}
	if bytes.Compare(lo, hi) >= 0 {
		return nil
	}

	srcs := []iter{newMemChunkIter(sn, lo, hi)}
	if sn.imm != nil {
		srcs = append(srcs, sn.imm.from(lo, sn.seq))
	}
	for i := len(sn.segs) - 1; i >= 0; i-- {
		at, err := sn.segs[i].seek(s.cache, lo)
		if err != nil {
			return err
		}
		it, err := sn.segs[i].from(s.cache, at)
		if err != nil {
			return err
		}
		srcs = append(srcs, it)
	}
	var keys, dead int64
	defer func() { scanKeys.Add(keys); scanDead.Add(dead) }()
	return eachKey(srcs, hi, func(real, _ []*row) (bool, error) {
		keys++
		if len(real) == 0 || !real[0].live() {
			dead++
			return true, nil
		}
		r := real[0]
		size := len(r.payload)
		if r.payload == nil {
			size = int(r.vlen)
		}
		out := recordWithCollection(collection, r)
		if load(out) {
			if err := s.payload(r); err != nil {
				return false, err
			}
			out.Payload = r.payload
		} else {
			out.Payload = nil
		}
		return visit(out, size), nil
	})
}

func (sn *Snapshot) Collections() []string {
	s := sn.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	limit := min(sn.cols, len(s.colNames))
	for _, name := range s.colNames[:limit] {
		if !strings.HasPrefix(name, "\x00") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
