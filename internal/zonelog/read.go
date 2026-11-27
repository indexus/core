package zonelog

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
)

// sources returns an iterator per memtable and segment, newest first, each
// placed at the first key not below lo. mu must be held.
func (s *Store) sources(lo []byte) ([]iter, error) {
	return s.sourcesAt(s.mem, s.imm, s.segs, s.visible, lo)
}

func (s *Store) sourcesAt(mem, imm *skiplist, segs []*segment, visible uint64, lo []byte) ([]iter, error) {
	srcs := []iter{mem.from(lo, visible)}
	if imm != nil {
		srcs = append(srcs, imm.from(lo, visible))
	}
	for i := len(segs) - 1; i >= 0; i-- {
		at, err := segs[i].seek(s.cache, lo)
		if err != nil {
			return nil, err
		}
		it, err := segs[i].from(s.cache, at)
		if err != nil {
			return nil, err
		}
		srcs = append(srcs, it)
	}
	return srcs, nil
}

// Get returns the live record with this id at this location.
// Pins a Snapshot so disk segment/payload I/O runs outside s.mu.
func (s *Store) Get(collection, location, id string) (Record, bool, error) {
	sn := s.Snapshot()
	defer sn.Close()
	return sn.Get(collection, location, id)
}

// Seq is the seq of the last write readers can see.
func (s *Store) Seq() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.visible
}

// Scan calls visit with the live records at or below a zone, in key order,
// until visit returns false. after, when not nil, is the last record of the
// previous page: the scan starts behind it.
func (s *Store) Scan(collection, zone string, after *Record, visit func(Record) bool) error {
	return s.ScanWith(collection, zone, after, nil, func(Record) bool { return true }, func(r Record, _ int) bool { return visit(r) })
}

// ScanWith is Scan that reads a payload only where load says so: a record
// visit gets without it has Payload nil. visit also gets the payload's length.
// Pins a Snapshot so disk segment/payload I/O runs outside s.mu.
func (s *Store) ScanWith(collection, zone string, after *Record, ids *IDRange, load func(Record) bool, visit func(Record, int) bool) error {
	sn := s.Snapshot()
	defer sn.Close()
	return sn.ScanWith(collection, zone, after, ids, load, visit)
}

// Collections lists the collections that have held records, by name.
func (s *Store) Collections() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, name := range s.colNames {
		if !strings.HasPrefix(name, "\x00") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// scan visits the live rows of [lo, hi), without their payloads.
func (s *Store) scan(lo, hi []byte, visit func(*row) (bool, error)) error {
	return s.scanAt(s.mem, s.imm, s.segs, s.visible, lo, hi, visit)
}

func (s *Store) scanAt(mem, imm *skiplist, segs []*segment, visible uint64, lo, hi []byte, visit func(*row) (bool, error)) error {
	srcs, err := s.sourcesAt(mem, imm, segs, visible, lo)
	if err != nil {
		return err
	}
	return eachKey(srcs, hi, func(real, _ []*row) (bool, error) {
		if len(real) == 0 || !real[0].live() {
			return true, nil
		}
		return visit(real[0])
	})
}

// span is what the rows of a key range add up to, and the key heads of its
// first and last row.
type span struct {
	sum         sums
	first, last []byte
}

func (a *span) ends(first, last []byte) {
	if a.first == nil || bytes.Compare(first, a.first) < 0 {
		a.first = first
	}
	if a.last == nil || bytes.Compare(last, a.last) > 0 {
		a.last = last
	}
}

// childScratch is the working memory of one Children call: 66 bounds, the
// 64 child heads they point into and 65 spans. Nothing of it outlives the
// call (Sum copies what it returns), so calls reuse it instead of allocating
// about 6 KiB each.
type childScratch struct {
	bounds [66][]byte
	heads  [64 * headLen]byte
	spans  [65]span
}

var childScratchPool = sync.Pool{New: func() any { return new(childScratch) }}

// release zeroes the scratch, keeping the capacity of each span's sums, and
// drops every reference to rows and pages.
func (c *childScratch) release() {
	clear(c.bounds[:])
	clear(c.heads[:])
	for i := range c.spans {
		a := &c.spans[i]
		clear(a.sum)
		a.sum, a.first, a.last = a.sum[:0], nil, nil
	}
	childScratchPool.Put(c)
}

// addSkiplistSpans folds one skiplist's visible rows in [bounds[0], bounds[last])
// into out. For a live memtable, s.mu.RLock must be held; an immutable imm
// skiplist may be read lock-free.
func addSkiplistSpans(l *skiplist, visible uint64, bounds [][]byte, out []span) {
	if l == nil {
		return
	}
	lo, hi := bounds[0], bounds[len(bounds)-1]
	i := 0
	for n := l.seek(lo, nil); n != nil && bytes.Compare(n.r.key, hi) < 0; n = n.next[0] {
		r := n.r.at(visible)
		if r == nil {
			continue
		}
		for bytes.Compare(r.key, bounds[i+1]) >= 0 {
			i++
		}
		out[i].sum.addRow(r)
		out[i].ends(r.key[:headLen], r.key[:headLen])
		if r.old != nil {
			out[i].sum.addRow(r.old)
		}
	}
}

// spansAt adds up the neighbouring key ranges [bounds[i], bounds[i+1]) for a
// pinned Snapshot. Only the mutable memtable is scanned under s.mu.RLock();
// immutable imm and all disk segment I/O run completely outside s.mu so cold
// reads never block Apply() or Snapshot().
func (sn *Snapshot) spans(bounds [][]byte) ([]span, error) {
	out := make([]span, len(bounds)-1)
	return out, sn.spansInto(bounds, out)
}

// spansInto is spans adding into out, which the caller zeroed: one span per
// pair of neighbouring bounds.
func (sn *Snapshot) spansInto(bounds [][]byte, out []span) error {
	s := sn.s
	lo, hi := bounds[0], bounds[len(bounds)-1]

	s.mu.RLock()
	addSkiplistSpans(sn.mem, sn.seq, bounds, out)
	s.mu.RUnlock()

	addSkiplistSpans(sn.imm, sn.seq, bounds, out)

	for _, g := range sn.segs {
		from, err := g.seek(s.cache, lo)
		if err != nil {
			return err
		}
		end, err := g.seek(s.cache, hi)
		if err != nil {
			return err
		}
		for i := 0; i < len(out) && from != end; i++ {
			to := end
			if i < len(out)-1 {
				if to, err = g.seek(s.cache, bounds[i+1]); err != nil {
					return err
				}
			}
			if from == to {
				continue
			}
			if err := g.sumRange(s.cache, from, to, &out[i].sum); err != nil {
				return err
			}
			first, last, err := g.ends(s.cache, from, to)
			if err != nil {
				return err
			}
			out[i].ends(first, last)
			from = to
		}
	}
	return nil
}

// Sum is the count and the additive sums of the records of a zone. Only is
// the one location that holds all of them, or "" when they are at several:
// the key rule of a /sets read (query.md).
type Sum struct {
	Count   int64
	Weight  float64
	Metrics []float64
	Only    string
}

// Children returns what a /sets read of a zone needs: [0] is the records
// placed exactly at the zone, [1+c] everything below its child c.
// Uses a pinned MVCC Snapshot so segment disk I/O runs outside s.mu.
func (s *Store) Children(collection, zone string) (out [65]Sum, err error) {
	sn := s.Snapshot()
	defer sn.Close()
	return sn.Children(collection, zone)
}

// Children reads the counts, metrics and exact locations at this snapshot.
func (sn *Snapshot) Children(collection, zone string) (out [65]Sum, err error) {
	s := sn.s
	s.mu.RLock()
	col, ok := s.cols[collection]
	s.mu.RUnlock()
	lo, hi, err := zoneRange(col, zone)
	if err != nil || !ok {
		return out, err
	}
	depth := int(lo[headLen-1])
	if depth >= locLen*8/6 {
		return out, fmt.Errorf("zonelog: zone %q has no children", zone)
	}
	// Bounds: the zone itself, its 64 children at depth+1, the zone's end.
	scratch := childScratchPool.Get().(*childScratch)
	defer scratch.release()
	bounds, heads, spans := scratch.bounds[:], scratch.heads[:], scratch.spans[:]
	bounds[0], bounds[65] = lo, hi
	for c := range 64 {
		b := heads[c*headLen : (c+1)*headLen : (c+1)*headLen]
		copy(b, lo)
		at := 6 * depth
		w := uint16(c) << 10 >> (at % 8)
		b[2+at/8] |= byte(w >> 8)
		if at/8+1 < locLen {
			b[2+at/8+1] |= byte(w)
		}
		b[headLen-1] = byte(depth + 1)
		bounds[1+c] = b
	}
	err = sn.spansInto(bounds, spans)
	for i, a := range spans {
		if err != nil || a.sum.at(1) == 0 {
			continue
		}
		metrics := make([]float64, max(a.sum.cols()-3, 0))
		for c := range metrics {
			metrics[c] = a.sum.at(3 + c)
		}
		out[i] = Sum{Count: int64(math.Round(a.sum.at(1))), Weight: a.sum.at(2), Metrics: metrics}
		out[i].Only, err = sn.only(a, bounds[i], bounds[i+1])
	}
	return out, err
}

// Total is what every record at or below a zone adds up to: the zone's slot
// in the children of its parent, or, for the root, its children added up.
func (s *Store) Total(collection, zone string) (Sum, error) {
	sn := s.Snapshot()
	defer sn.Close()
	return sn.Total(collection, zone)
}

// Total is Store.Total read from this snapshot.
func (sn *Snapshot) Total(collection, zone string) (Sum, error) {
	if zone == "@" || zone == "" {
		children, err := sn.Children(collection, "@")
		return totalOf(children), err
	}
	parent := cmp.Or(zone[:len(zone)-1], "@")
	children, err := sn.Children(collection, parent)
	if err != nil {
		return Sum{}, err
	}
	at := charIndex[zone[len(zone)-1]]
	if at < 0 {
		return Sum{}, fmt.Errorf("zone %q: bad character", zone)
	}
	return children[1+at], nil
}

func totalOf(children [65]Sum) Sum {
	var out Sum
	parts := 0
	for _, part := range children {
		if part.Count == 0 {
			continue
		}
		parts++
		out.Count += part.Count
		out.Weight += part.Weight
		for len(out.Metrics) < len(part.Metrics) {
			out.Metrics = append(out.Metrics, 0)
		}
		for i, v := range part.Metrics {
			out.Metrics[i] += v
		}
		out.Only = part.Only
	}
	if parts != 1 {
		out.Only = ""
	}
	return out
}

// only names the location of a range whose live records all sit at one
// location, or "". When the first and last row of the range are at one
// location, that is it. When they are not and every row is a live record,
// there are several. Otherwise the range holds tombstones or replaced
// versions, and the records at the location of the first live record must
// count as many as the whole range.
func (sn *Snapshot) only(a span, lo, hi []byte) (string, error) {
	head := a.first
	if !bytes.Equal(a.first, a.last) {
		if a.sum.at(0) == a.sum.at(1) {
			return "", nil
		}
		head = nil
		// Scan memtable in bounded chunks and disk segments completely outside s.mu.
		srcs := []iter{newMemChunkIter(sn, lo, hi)}
		if sn.imm != nil {
			srcs = append(srcs, sn.imm.from(lo, sn.seq))
		}
		for i := len(sn.segs) - 1; i >= 0; i-- {
			at, err := sn.segs[i].seek(sn.s.cache, lo)
			if err != nil {
				return "", err
			}
			it, err := sn.segs[i].from(sn.s.cache, at)
			if err != nil {
				return "", err
			}
			srcs = append(srcs, it)
		}
		err := eachKey(srcs, hi, func(real, _ []*row) (bool, error) {
			if len(real) == 0 || !real[0].live() {
				return true, nil
			}
			if real[0].flags&flagUncounted != 0 {
				return true, nil
			}
			head = real[0].key[:headLen]
			return false, nil
		})
		if err != nil || head == nil {
			return "", err
		}
		end := bytes.Clone(head)
		end[headLen-1]++ // the same location one level deeper: behind every id
		at, err := sn.spans([][]byte{head, end})
		if err != nil || at[0].sum.at(1) != a.sum.at(1) {
			return "", err
		}
	}
	return locString(head[2:2+locLen], int(head[headLen-1])), nil
}
