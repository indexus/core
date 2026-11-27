package zonelog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

var ErrNotEmpty = errors.New("zonelog: the zone holds live records")

// Bulk is a segment written from records given in key order, for one
// collection at or below one zone. No read sees it before Commit, whose
// manifest rename is the cut: a crash before leaves nothing, after it the
// whole batch. Without replace the zone must hold no live record; with
// replace the segment also retracts every live record of the zone and
// deletes those the batch does not write again.
//
// The caller keeps every other write out of the zone until Commit or Abort.
// Merges wait meanwhile: they would close the segments a replace reads.
type Bulk struct {
	s        *Store
	w        *segWriter
	id, seq  uint64
	col      uint16
	lo, hi   []byte
	last     []byte
	old      *newest
	next     *row   // the next live record of the zone not yet passed
	tail     []*row // extra records that sort after the zone
	done     bool
	replaced int64
}

// BeginBulk starts a bulk segment. expect is about how many records come,
// for the key filter.
func (s *Store) BeginBulk(collection, zone string, replace bool, expect int64) (*Bulk, error) {
	if err := s.Flush(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.bulk {
		s.mu.Unlock()
		return nil, fmt.Errorf("zonelog: a bulk segment is already being written")
	}
	b := &Bulk{s: s}
	col, err := s.collection(collection)
	if err == nil {
		b.col = col
		b.lo, b.hi, err = zoneRange(col, zone)
	}
	if err == nil && !replace {
		err = s.scan(b.lo, b.hi, func(*row) (bool, error) { return false, ErrNotEmpty })
	}
	var srcs []iter
	if err == nil && replace {
		srcs, err = s.sources(b.lo)
	}
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.bulk = true
	s.seq++
	b.seq, b.id = s.seq, s.nextFile
	s.nextFile++
	s.pending = appendFrame(s.pending, b.seq, 1, nil)
	s.mu.Unlock()
	if replace {
		b.old = &newest{srcs: srcs}
		if b.next, err = b.old.next(b.hi); err != nil {
			b.Abort()
			return nil, err
		}
	}
	if b.w, err = newSegWriter(s.fs, b.id, uint64(max(expect, 0))+1); err != nil {
		b.Abort()
		return nil, err
	}
	return b, nil
}

// AddHead writes one record, given by the head of its key (location bits,
// left aligned, and depth: key bytes 2 to 14) and its id, as a prepared part
// holds it: no location to parse. Its key must be in the zone and after the
// last.
func (b *Bulk) AddHead(head, id []byte, weight float64, metrics []float64, payload []byte, raw bool) error {
	if len(head) != headLen-2 || len(id) == 0 || len(id) > maxID || int(head[locLen]) > locLen*8/6 {
		return fmt.Errorf("zonelog: bulk record: bad head or id")
	}
	// Bits past the location's characters are zero in a key.
	for bit := 6 * int(head[locLen]); bit < 8*locLen; bit++ {
		if head[bit/8]&(0x80>>(bit%8)) != 0 {
			return fmt.Errorf("zonelog: bulk record: bits past the location")
		}
	}
	key := make([]byte, 2, headLen+len(id))
	binary.BigEndian.PutUint16(key, b.col)
	return b.add(append(append(key, head...), id...), weight, metrics, payload, raw)
}

func (b *Bulk) add(key []byte, weight float64, metrics []float64, payload []byte, raw bool) error {
	switch {
	case len(metrics) > MaxMetrics:
		return fmt.Errorf("zonelog: bulk record with %d metrics", len(metrics))
	case bytes.Compare(key, b.lo) < 0 || bytes.Compare(key, b.hi) >= 0:
		return fmt.Errorf("zonelog: bulk record %q is outside the zone", key[headLen:])
	case b.last != nil && bytes.Compare(key, b.last) <= 0:
		return fmt.Errorf("zonelog: bulk record %q is not after the previous one", key[headLen:])
	}
	b.last = key
	if err := b.pass(key); err != nil {
		return err
	}
	if weight == 0 {
		weight = 1
	}
	// The writer holds rows until their page is full: they own their bytes.
	row := &row{key: key, seq: b.seq, weight: weight, metrics: slices.Clone(metrics), payload: bytes.Clone(payload)}
	if raw {
		row.flags = flagBinary
	}
	if err := b.w.add(row); err != nil {
		return err
	}
	if b.next != nil && bytes.Equal(b.next.key, key) {
		return b.retract()
	}
	return nil
}

// pass deletes the old records before key that the batch does not write.
func (b *Bulk) pass(key []byte) error {
	for b.next != nil && (key == nil || bytes.Compare(b.next.key, key) < 0) {
		if err := b.w.add(&row{key: b.next.key, flags: flagDeleted, seq: b.seq}); err != nil {
			return err
		}
		if err := b.retract(); err != nil {
			return err
		}
	}
	return nil
}

// retract cancels the sums of the old version under the cursor and moves on.
func (b *Bulk) retract() error {
	if err := b.w.add(b.next.retraction()); err != nil {
		return err
	}
	b.replaced++
	var err error
	b.next, err = b.old.next(b.hi)
	return err
}

// Also writes records of another collection, such as zonelog.Control, in
// the same cut. It comes before the first Add. Existing extra keys are
// replaced at this cut's seq, just like records updated by Apply.
func (b *Bulk) Also(records ...Record) error {
	if b.last != nil {
		return fmt.Errorf("zonelog: bulk extra records come before the first record")
	}
	var head []*row
	for _, r := range records {
		b.s.mu.Lock()
		col, err := b.s.collection(r.Collection)
		b.s.mu.Unlock()
		if err != nil {
			return err
		}
		key, err := makeKey(col, r.Location, r.ID)
		if err != nil {
			return err
		}
		if bytes.Compare(key, b.lo) >= 0 && bytes.Compare(key, b.hi) < 0 {
			return fmt.Errorf("zonelog: bulk extra record inside the zone")
		}
		x := &row{key: key, seq: b.seq, weight: 1, metrics: slices.Clone(r.Metrics), payload: bytes.Clone(r.Payload)}
		if bytes.Compare(key, b.lo) < 0 {
			head = append(head, x)
		} else {
			b.tail = append(b.tail, x)
		}
	}
	slices.SortFunc(head, func(x, y *row) int { return bytes.Compare(x.key, y.key) })
	for _, x := range head {
		if err := b.w.add(x); err != nil {
			return err
		}
	}
	return nil
}

// Replaced is how many old records the segment retracts.
func (b *Bulk) Replaced() int64 { return b.replaced }

// Commit installs the segment.
func (b *Bulk) Commit() error {
	if err := b.pass(nil); err != nil {
		b.Abort()
		return err
	}
	if err := b.finish(); err != nil {
		b.Abort()
		return err
	}
	s := b.s
	g, err := openSegment(s.fs, b.id)
	if err == nil {
		err = s.sync(b.seq)
	}
	if err != nil {
		b.Abort()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segs = append(s.segs, g)
	s.bulk, b.done = false, true
	if err = s.writeManifest(); err == nil {
		err = s.dropPayloadFiles()
	}
	if err != nil {
		s.err = err
		return err
	}
	if lo, hi := s.mergeable(); lo != hi {
		s.startWork()
	}
	return nil
}

// finish adds the extra records after the zone, and closes the file.
func (b *Bulk) finish() error {
	slices.SortFunc(b.tail, func(x, y *row) int { return bytes.Compare(x.key, y.key) })
	for _, x := range b.tail {
		if err := b.w.add(x); err != nil {
			return err
		}
	}
	return b.w.finish(!b.s.opt.NoSync)
}

// Abort forgets the segment.
func (b *Bulk) Abort() {
	if b.done {
		return
	}
	b.done = true
	if b.w != nil {
		b.w.abort()
	}
	s := b.s
	s.fs.Remove(segName(b.id))
	s.fs.Remove(valName(b.id))
	s.mu.Lock()
	s.bulk = false
	s.mu.Unlock()
}

// newest reads the newest version of each key from sources given newest
// first, and returns the live ones.
type newest struct{ srcs []iter }

func (n *newest) next(hi []byte) (*row, error) {
	for {
		var key []byte
		for _, it := range n.srcs {
			if r := it.row(); r != nil && (key == nil || bytes.Compare(r.key, key) < 0) {
				key = r.key
			}
		}
		if key == nil || bytes.Compare(key, hi) >= 0 {
			return nil, nil
		}
		var top *row
		for _, it := range n.srcs {
			for r := it.row(); r != nil && bytes.Equal(r.key, key); r = it.row() {
				if top == nil && r.flags&flagRetract == 0 {
					top = r
				}
				if err := it.next(); err != nil {
					return nil, err
				}
			}
		}
		if top != nil && top.live() {
			return top, nil
		}
	}
}
