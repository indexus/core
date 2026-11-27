package zonelog

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	format = "zonelog-2" // 2: sums are Neumaier pairs
	// Control is the collection of the caller's own state: authority, fences,
	// transfer progress. Its records commit in the same frame as the data of
	// their batch. receipts is collection number 0 of every store: it held
	// the receipts of replaced writes of a request id, which no caller gives.
	Control  = "\x00control"
	receipts = "\x00receipts"
)

// Options bound what the store keeps. Zero values take the defaults.
type Options struct {
	// Entropy supplies fresh disk identities. Nil uses crypto/rand.Reader.
	// Simulations inject a seeded reader; production must retain secure entropy.
	Entropy     io.Reader
	FS          FS    // where the files are (the directory given to Open)
	MemoryBytes int   // resident memory to stay under (256 MB); see Open
	LogRetain   int64 // bytes of flushed log kept besides the segments (64 MB)
	BlobBytes   int   // payloads from this size are written once; merges point at them (4096)
	NoSync      bool  // benchmarks only: no fsync
}

// Store is one node's records. mu guards the memtables, the segment list and
// the counters; syncMu serialises log writes.
type Store struct {
	fs  FS
	opt Options
	id  string

	mu       sync.RWMutex
	flushed  *sync.Cond
	mem, imm *skiplist
	segs     []*segment // oldest first
	cols     map[string]uint16
	colNames []string
	marks    [][2]uint64 // kept as read: the receipt expiry of earlier releases
	seq      uint64      // last assigned
	visible  uint64      // last synced: what readers see
	durable  uint64      // last seq held by a segment
	nextFile uint64
	pending  []byte // frames not yet written to the log
	timing   timings
	working  bool // merge worker
	flushing bool // independent flush worker; at most one immutable memtable
	bulk     bool // a bulk segment is being written: no merge starts
	work     sync.WaitGroup
	cache    *cache
	err      error
	// snapMu guards snapshots and retiredSegs so Snapshot()/Close() never need
	// an exclusive s.mu.Lock() on the read path. Lock order: s.mu -> s.snapMu.
	snapMu      sync.Mutex
	snapshots   []uint64     // sorted active snapshot seqs (MVCC read views)
	retiredSegs []retiredSeg // merged-away segments held open until older snapshots close

	valMu sync.Mutex
	vals  map[uint64]valFile // every payload file a segment points at, open

	syncMu sync.Mutex
	logs   []logFile
	log    File
	synced uint64
}

type manifest struct {
	Format, ID  string
	Next        uint64
	Durable     uint64
	Segments    []uint64
	Collections []string
	Marks       [][2]uint64
}

// Open opens the store of a directory, or creates it. It refuses another
// format, removes what an unfinished flush or merge left, and replays the log.
//
// MemoryBytes is shared out as: a sixteenth for each of the two memtables,
// an eighth for the page cache and the indexes together. That is about a
// quarter of it alive; the rest is for the frames and records in flight and
// for the garbage collector, which lets the heap reach twice what is alive.
func Open(dir string, opt Options) (*Store, error) {
	if opt.FS == nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		opt.FS = Dir(dir)
	}
	opt.MemoryBytes = cmp.Or(opt.MemoryBytes, 256<<20)
	opt.LogRetain = cmp.Or(opt.LogRetain, 64<<20)
	opt.BlobBytes = cmp.Or(opt.BlobBytes, 4096)
	s := &Store{fs: opt.FS, opt: opt, mem: newSkiplist(), cols: map[string]uint16{}, nextFile: 1}
	s.flushed = sync.NewCond(&s.mu)
	s.cache, s.vals = newCache(opt.MemoryBytes/8), map[uint64]valFile{}
	names, err := s.fs.List()
	if err != nil {
		return nil, err
	}
	var m manifest
	if slices.Contains(names, "MANIFEST") {
		raw, err := readAll(s.fs, "MANIFEST")
		if err == nil {
			err = json.Unmarshal(raw, &m)
		}
		if err != nil || m.Format != format {
			return nil, fmt.Errorf("zonelog: not a %s store: %q %v", format, m.Format, err)
		}
		s.id, s.nextFile, s.durable, s.seq, s.colNames, s.marks = m.ID, m.Next, m.Durable, m.Durable, m.Collections, m.Marks
	} else {
		var id [16]byte
		entropy := opt.Entropy
		if entropy == nil {
			entropy = rand.Reader
		}
		if _, err := io.ReadFull(entropy, id[:]); err != nil {
			return nil, fmt.Errorf("zonelog identity: %w", err)
		}
		s.id, s.colNames = hex.EncodeToString(id[:]), []string{receipts}
		if err := s.writeManifest(); err != nil {
			return nil, err
		}
	}
	for i, name := range s.colNames {
		s.cols[name] = uint16(i)
	}
	for _, id := range m.Segments {
		g, err := openSegment(s.fs, id)
		if err != nil {
			return nil, err
		}
		s.segs = append(s.segs, g)
	}
	// A segment the manifest does not name, or a payload file no segment
	// points at, is a flush, a merge or an ingest that did not commit.
	for _, name := range names {
		var id uint64
		fmt.Sscanf(name, "%d", &id)
		switch {
		case strings.HasSuffix(name, ".seg") && !slices.Contains(m.Segments, id), strings.HasSuffix(name, ".tmp"):
			s.fs.Remove(name)
		case strings.HasSuffix(name, ".val"):
			s.vals[id] = valFile{}
		}
	}
	if err := s.dropPayloadFiles(); err != nil {
		return nil, err
	}
	if err := s.recover(names); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) writeManifest() error {
	m := manifest{Format: format, ID: s.id, Next: s.nextFile, Durable: s.durable, Collections: s.colNames, Marks: s.marks}
	for _, g := range s.segs {
		m.Segments = append(m.Segments, g.id)
	}
	raw, _ := json.Marshal(m)
	f, err := s.fs.Create("MANIFEST.tmp")
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err == nil && !s.opt.NoSync {
		err = f.Sync()
	}
	f.Close()
	if err == nil {
		err = s.fs.Rename("MANIFEST.tmp", "MANIFEST")
	}
	if err == nil && !s.opt.NoSync {
		err = s.fs.Sync() // the rename is the commit, and it is not durable before this
	}
	return err
}

// dropPayloadFiles opens the payload files the segments point at, removes
// the others, and gives the page cache what the indexes leave of its share
// of the memory budget. Called after every change of the segment list.
func (s *Store) dropPayloadFiles() error {
	memory := 0
	used := map[uint64]bool{}
	for _, g := range s.segs {
		memory += g.memory()
		for id := range g.Files {
			used[id] = true
		}
	}
	s.snapMu.Lock()
	for _, r := range s.retiredSegs {
		for id := range r.seg.Files {
			used[id] = true
		}
	}
	s.snapMu.Unlock()
	s.cache.mu.Lock()
	s.cache.limit = max(s.opt.MemoryBytes/8-memory, s.opt.MemoryBytes/64)
	s.cache.mu.Unlock()
	s.valMu.Lock()
	defer s.valMu.Unlock()
	for id, v := range s.vals {
		if !used[id] {
			if v.File != nil {
				v.Close()
			}
			delete(s.vals, id)
			s.fs.Remove(valName(id))
		}
	}
	for id := range used {
		if s.vals[id].File == nil {
			f, err := s.fs.Open(valName(id))
			if err != nil {
				return err
			}
			v, err := openVal(f, id)
			if err != nil {
				f.Close()
				return err
			}
			s.vals[id] = v
		}
	}
	return nil
}

// payload reads the payload of a row that only points at it.
func (s *Store) payload(r *row) error {
	if r.payload != nil || r.vlen == 0 {
		return nil
	}
	s.valMu.Lock()
	f := s.vals[r.val]
	s.valMu.Unlock()
	payload := make([]byte, r.vlen)
	if err := f.read(s.cache, payload, r.voff); err != nil {
		return err
	}
	r.payload = payload
	return nil
}

// collection returns the number of a collection, writing the manifest first
// when it is new: keys in the log name collections by number.
func (s *Store) collection(name string) (uint16, error) {
	if n, ok := s.cols[name]; ok {
		return n, nil
	}
	s.colNames = append(s.colNames, name)
	s.cols[name] = uint16(len(s.colNames) - 1)
	return s.cols[name], s.writeManifest()
}

// minVisible is the oldest seq any active reader or snapshot can still observe.
// mu must be held.
func (s *Store) minVisible() uint64 {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if len(s.snapshots) > 0 && s.snapshots[0] < s.visible {
		return s.snapshots[0]
	}
	return s.visible
}

// lookup returns the newest version of key that is not above visible,
// tombstones included.
func (s *Store) lookup(key []byte, visible uint64) (*row, error) {
	return s.lookupAt(s.mem, s.imm, s.segs, key, visible)
}

func (s *Store) lookupAt(mem, imm *skiplist, segs []*segment, key []byte, visible uint64) (*row, error) {
	if r := mem.get(key).at(visible); r != nil {
		return r, nil
	}
	if r := imm.get(key).at(visible); r != nil {
		return r, nil
	}
	for i := len(segs) - 1; i >= 0; i-- {
		if r, err := segs[i].get(s.cache, key); r != nil || err != nil {
			return r, err
		}
	}
	return nil, nil
}

// unchanged reports whether writing r over prev would change nothing: the
// same bytes, or a delete of a record that does not exist.
func (s *Store) unchanged(prev, r *row) (bool, error) {
	if prev == nil {
		return r.flags&flagDeleted != 0, nil
	}
	size := len(prev.payload)
	if prev.payload == nil {
		size = int(prev.vlen)
	}
	if prev.flags != r.flags || prev.weight != r.weight || !slices.Equal(prev.metrics, r.metrics) || size != len(r.payload) {
		return false, nil
	}
	err := s.payload(prev)
	return bytes.Equal(prev.payload, r.payload), err
}

// prepare turns a record into a row over prev, the newest stored version of
// its key, or into nil when it changes nothing: the bytes equal the stored
// record, or it deletes a record that does not exist.
func (s *Store) prepare(rec Record) (r, prev *row, err error) {
	col, err := s.collection(rec.Collection)
	if err != nil {
		return nil, nil, err
	}
	r, err = mutation(rec, col)
	if err != nil {
		return nil, nil, err
	}
	if prev, err = s.lookup(r.key, ^uint64(0)); err != nil {
		return nil, nil, err
	}
	if same, err := s.unchanged(prev, r); same || err != nil {
		return nil, nil, err
	}
	return r, prev, nil
}

// place puts a row in the memtable over prev, the newest stored version of
// its key. When prev is live outside the memtable the row carries its
// retraction, so sums stay additive.
func (s *Store) place(r, prev *row) {
	if cur := s.mem.get(r.key); cur != nil {
		r.old = cur.old
	} else if prev != nil && prev.live() {
		r.old = prev.retraction()
	}
	s.mem.put(r, s.minVisible())
}

// Apply commits a batch of writes: all of it or none of it survives a
// crash, and it is on disk and visible to readers when Apply returns. A
// write that changes nothing takes no seq.
func (s *Store) Apply(batch []Record) error {
	// Cold key/payload reads happen on a pinned snapshot, not under mu.
	// A concurrent writer invalidates this one preparation; the locked
	// path below then decides the batch against the newest assigned seq.
	snapshot := s.Snapshot()
	defer snapshot.Close()
	rows, prevs, prepared, err := s.prepareSnapshot(snapshot, batch)
	if err != nil {
		return err
	}
	started := time.Now()
	s.mu.Lock()
	s.timing.lock.Observe(time.Since(started))
	for s.err == nil && s.mem.bytes >= s.opt.MemoryBytes/16 {
		if s.imm == nil {
			s.imm, s.mem = s.mem, newSkiplist()
			s.startWork()
			break
		}
		started := time.Now()
		s.flushed.Wait() // back-pressure: the previous memtable is still being written
		s.timing.memtable.Observe(time.Since(started))
	}
	err = s.err
	if err == nil {
		// Persist every new collection ID before the batch can enter the log.
		// A zone install may introduce hundreds of collections: one manifest
		// commit for the batch avoids one durable rename per collection.
		before := len(s.colNames)
		for _, rec := range batch {
			if _, exists := s.cols[rec.Collection]; !exists {
				s.cols[rec.Collection] = uint16(len(s.colNames))
				s.colNames = append(s.colNames, rec.Collection)
			}
		}
		if len(s.colNames) != before {
			err = s.writeManifest()
			if err != nil {
				// The rename may have reached disk. Until reopen determines
				// its result, no later write may use the provisional IDs.
				s.err = err
			}
		}
	}
	// Publish only if preparation still describes this exact logical state.
	// Segment compaction is harmless: the snapshot pins the previous rows.
	if !prepared || s.seq != snapshot.seq || s.bulk {
		rows, prevs = make([]*row, len(batch)), make([]*row, len(batch))
		for i := 0; i < len(batch) && err == nil; i++ {
			rows[i], prevs[i], err = s.prepare(batch[i])
		}
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	first := s.seq + 1
	placed := rows[:0]
	for i, r := range rows {
		if r == nil {
			continue // changes nothing
		}
		s.seq++
		r.seq = s.seq
		s.place(r, prevs[i])
		placed = append(placed, r)
	}
	if len(placed) > 0 {
		s.pending = appendFrame(s.pending, first, uint32(len(placed)), placed)
	}
	upto := s.seq
	s.mu.Unlock()
	return s.sync(upto)
}

// startWork gives flushes their own worker: a long merge must not prevent
// recycling the immutable memtable. There is still only one merge at a time,
// and only one immutable memtable, so the memory budget is unchanged.
// mu must be held. Workers stop when idle; no timers or permanent goroutines.
func (s *Store) startWork() {
	if s.imm != nil && !s.flushing {
		s.flushing = true
		s.work.Add(1)
		go func() {
			defer s.work.Done()
			for s.workOnce(true) {
			}
		}()
	}
	if !s.working {
		s.working = true
		s.work.Add(1)
		go func() {
			defer s.work.Done()
			for s.workOnce(false) {
			}
		}()
	}
}

// Only the flush worker consumes imm. The merge worker captures a range of
// existing segments; flush publication only appends to that range, so lo/hi
// remain valid until this single merge publishes under mu.
func (s *Store) workOnce(flush bool) bool {
	s.mu.Lock()
	var imm *skiplist
	lo, hi := 0, 0
	if flush {
		imm = s.imm
	} else if !s.bulk {
		lo, hi = s.mergeable()
	}
	if s.err != nil || imm == nil && lo == hi {
		if flush {
			s.flushing = false
		} else {
			s.working = false
		}
		s.flushed.Broadcast()
		s.mu.Unlock()
		return false
	}
	id, inputs := s.nextFile, s.segs[lo:hi:hi]
	s.nextFile++
	b := build{id: id, bottom: imm == nil && lo == 0, rewrite: s.deadFiles()}
	s.mu.Unlock()

	var err error
	if imm != nil {
		// Readers must not see a segment before its rows are synced.
		if err = s.sync(imm.maxSeq); err == nil {
			b.srcs, b.rows = []iter{imm.from(nil, ^uint64(0))}, uint64(2*imm.rows)
		}
	}
	for i := len(inputs) - 1; i >= 0 && err == nil; i-- {
		var it *segIter
		it, err = inputs[i].from(nil, pos{})
		b.srcs, b.rows = append(b.srcs, it), b.rows+inputs[i].Rows
	}
	var g *segment
	started := time.Now()
	if err == nil {
		g, err = s.build(b)
	}
	if imm != nil {
		s.timing.flush.Observe(time.Since(started))
	} else {
		s.timing.merge.Observe(time.Since(started))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	started = time.Now()
	defer func() { s.timing.publish.Observe(time.Since(started)) }()
	if err == nil && imm != nil {
		s.segs, s.durable, s.imm = append(s.segs, g), imm.maxSeq, nil
	} else if err == nil {
		s.segs = append(append(append([]*segment(nil), s.segs[:lo]...), g), s.segs[hi:]...)
	}
	if err == nil {
		err = s.writeManifest()
	}
	if err == nil {
		// Only now are the inputs garbage: the manifest no longer names them.
		// If snapshots are open, defer closing/removing until the last one closes.
		s.snapMu.Lock()
		for _, old := range inputs {
			s.retiredSegs = append(s.retiredSegs, retiredSeg{seg: old, retiredAt: s.visible})
		}
		reclaimable := s.takeReclaimableLocked()
		s.snapMu.Unlock()
		for _, old := range reclaimable {
			old.f.Close()
			s.fs.Remove(segName(old.id))
		}
		err = s.dropPayloadFiles()
	}
	if err != nil {
		s.err = err
	}
	// A flush may have made a merge eligible after the merge worker stopped.
	if flush && err == nil {
		s.startWork()
	}
	s.flushed.Broadcast()
	return true
}

// deadFiles is the payload files that segments point at for less than two
// thirds of their bytes. A merge copies the payloads it meets out of them,
// so that they can be removed.
func (s *Store) deadFiles() map[uint64]bool {
	pointed, dead := map[uint64]int64{}, map[uint64]bool{}
	for _, g := range s.segs {
		for id, n := range g.Files {
			pointed[id] += n
		}
	}
	for id, n := range pointed {
		dead[id] = n*3 < s.vals[id].size*2
	}
	return dead
}

// mergeable picks segments [lo, hi) to merge. In order: a segment that
// points into a dead payload file, alone; all of them when replaced versions
// and tombstones outnumber half the live records; four or more neighbours
// of the same size class, where a class is a factor of four. Each row is
// then rewritten about log4(data / memtable) times; large payloads are not.
func (s *Store) mergeable() (lo, hi int) {
	dead, rows, live := s.deadFiles(), 0.0, 0.0
	for i, g := range s.segs {
		for id := range g.Files {
			if dead[id] {
				return i, i + 1
			}
		}
		for _, b := range g.blocks {
			rows, live = rows+b.sum.at(0), live+b.sum.at(1)
		}
	}
	// One segment is not merged for waste: uncounted rows (live, and
	// counted as rows only) would make that merge start again forever.
	if rows-live > live && len(s.segs) > 1 {
		return 0, len(s.segs)
	}
	class := func(g *segment) int { return bits.Len64(uint64(g.bytes)/uint64(s.opt.MemoryBytes/32+1)) / 2 }
	for hi = len(s.segs); hi > 0; hi = lo {
		for lo = hi - 1; lo > 0 && class(s.segs[lo-1]) == class(s.segs[hi-1]); lo-- {
		}
		if hi-lo >= 4 {
			return lo, hi
		}
	}
	return 0, 0
}

// eachKey merges sources given newest first and calls visit once per key
// with its versions, newest first: records (and tombstones) in real, their
// retractions in retract. A non-nil hi ends the merge before that key, even
// when only retractions (no real row) exist there.
func eachKey(srcs []iter, hi []byte, visit func(real, retract []*row) (bool, error)) error {
	var real, retract []*row
	for {
		var key []byte
		for _, it := range srcs {
			if r := it.row(); r != nil && (key == nil || bytes.Compare(r.key, key) < 0) {
				key = r.key
			}
		}
		if key == nil || hi != nil && bytes.Compare(key, hi) >= 0 {
			return nil
		}
		real, retract = real[:0], retract[:0]
		for _, it := range srcs {
			for r := it.row(); r != nil && bytes.Equal(r.key, key); r = it.row() {
				if r.flags&flagRetract != 0 {
					retract = append(retract, r)
				} else {
					real = append(real, r)
				}
				if err := it.next(); err != nil {
					return err
				}
			}
		}
		if more, err := visit(real, retract); !more || err != nil {
			return err
		}
	}
}

// build is one segment to write: its sources, newest first, and what to
// leave out.
type build struct {
	id      uint64
	srcs    []iter
	rows    uint64          // at most this many
	bottom  bool            // the oldest segment is among the sources: tombstones end here
	rewrite map[uint64]bool // payload files to copy out of
}

// build writes one segment. Of each key it keeps the newest version, and the
// retractions whose version is not among the sources: that version lives in
// an older segment and still needs cancelling.
func (s *Store) build(b build) (*segment, error) {
	w, err := newSegWriter(s.fs, b.id, b.rows)
	if err != nil {
		return nil, err
	}
	err = eachKey(b.srcs, nil, func(real, retract []*row) (bool, error) {
		if len(real) > 0 {
			r := real[0]
			if r.payload == nil && (int(r.vlen) < s.opt.BlobBytes || b.rewrite[r.val]) {
				if err := s.payload(r); err != nil {
					return false, err
				}
			}
			// A receipt row of an earlier release is in no read: merges drop it.
			if !(b.bottom && r.flags&flagDeleted != 0) && r.flags != flagReceipt {
				if err := w.add(r); err != nil {
					return false, err
				}
			}
		}
		for _, t := range retract {
			if !slices.ContainsFunc(real, func(r *row) bool { return r.seq == t.seq }) {
				if err := w.add(t); err != nil {
					return false, err
				}
			}
		}
		return true, nil
	})
	if err == nil {
		err = w.finish(!s.opt.NoSync)
	} else {
		w.abort()
	}
	if err == nil && !s.opt.NoSync {
		err = s.fs.Sync()
	}
	if err != nil {
		return nil, err
	}
	return openSegment(s.fs, b.id)
}

// Flush writes the memtable to a segment and waits for merges to settle.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.err == nil && (s.imm != nil || s.flushing || s.working || s.mem.rows > 0) {
		if s.imm == nil && s.mem.rows > 0 {
			s.imm, s.mem = s.mem, newSkiplist()
		}
		s.startWork()
		s.flushed.Wait()
	}
	return s.err
}

// Close stops background work and closes the files. The memtable stays in
// the log and is rebuilt by the next Open.
func (s *Store) Close() error {
	s.work.Wait()
	err := s.sync(^uint64(0))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.segs {
		g.f.Close()
	}
	s.snapMu.Lock()
	retired := s.retiredSegs
	s.retiredSegs = nil
	s.snapMu.Unlock()
	for _, r := range retired {
		r.seg.f.Close()
		s.fs.Remove(segName(r.seg.id))
	}
	for _, v := range s.vals {
		v.Close()
	}
	s.log.Close()
	if s.err == nil {
		s.err = errors.New("zonelog: closed")
	}
	return err
}

// Stats is what the store holds on disk and in memory.
type Stats struct {
	Segments, Logs int
	SegmentBytes   int64 // segments and payload files
	LogBytes       int64
	Rows           uint64 // rows in segments, retractions and tombstones included
	IndexBytes     int    // what segments keep in memory; their indexes and filters are read through the cache
	CacheBytes     int    // pages, index blocks, filter chunks and payload frames in the cache
	Memtable       int
}

func (s *Store) Stats() Stats {
	s.syncMu.Lock()
	out := Stats{Logs: len(s.logs)}
	for _, lf := range s.logs {
		out.LogBytes += lf.size
	}
	s.syncMu.Unlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out.Segments, out.Memtable = len(s.segs), s.mem.bytes
	for _, g := range s.segs {
		out.SegmentBytes, out.Rows, out.IndexBytes = out.SegmentBytes+g.bytes, out.Rows+g.Rows, out.IndexBytes+g.memory()
	}
	for _, v := range s.vals {
		out.SegmentBytes += v.size
	}
	s.cache.mu.Lock()
	out.CacheBytes = s.cache.used
	s.cache.mu.Unlock()
	return out
}
