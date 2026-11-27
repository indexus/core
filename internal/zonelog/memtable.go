package zonelog

import "bytes"

const maxLevel = 16

// skiplist is the memtable: rows sorted by key. One writer at a time; readers
// are excluded by the store lock.
type skiplist struct {
	head   node
	rnd    uint64
	bytes  int
	rows   int
	maxSeq uint64
}

type node struct {
	r    *row
	next []*node
}

func newSkiplist() *skiplist {
	return &skiplist{head: node{next: make([]*node, maxLevel)}, rnd: 0x9E3779B97F4A7C15}
}

// seek returns the first node whose key is not below key, and fills prev
// with the last node before it on every level.
func (l *skiplist) seek(key []byte, prev *[maxLevel]*node) *node {
	at := &l.head
	for level := maxLevel - 1; level >= 0; level-- {
		for at.next[level] != nil && bytes.Compare(at.next[level].r.key, key) < 0 {
			at = at.next[level]
		}
		if prev != nil {
			prev[level] = at
		}
	}
	return at.next[0]
}

// get returns the newest row of key, synced or not.
func (l *skiplist) get(key []byte) *row {
	if l == nil {
		return nil
	}
	if n := l.seek(key, nil); n != nil && bytes.Equal(n.r.key, key) {
		return n.r
	}
	return nil
}

// put places r over any row of the same key. The replaced row stays below r
// for readers, and drops what was below it once readers can see it.
func (l *skiplist) put(r *row, visible uint64) {
	var prev [maxLevel]*node
	// What the row holds, plus the row, its node and their slice headers.
	l.bytes += len(r.key) + len(r.payload) + 8*len(r.metrics) + 320
	l.rows++
	l.maxSeq = max(l.maxSeq, r.seq)
	if n := l.seek(r.key, &prev); n != nil && bytes.Equal(n.r.key, r.key) {
		if r.below = n.r; n.r.seq <= visible {
			n.r.below = nil
		}
		n.r = r
		return
	}
	level := 1
	for l.rnd ^= l.rnd << 13; level < maxLevel; level++ {
		l.rnd ^= l.rnd >> 7
		l.rnd ^= l.rnd << 17
		if l.rnd&3 != 0 {
			break
		}
	}
	n := &node{r: r, next: make([]*node, level)}
	for i := range level {
		n.next[i] = prev[i].next[i]
		prev[i].next[i] = n
	}
}

// iter walks rows in key order. A source of rows for reads and merges: a
// memtable yields a row and then its retraction, a segment its stored rows.
type iter interface {
	row() *row
	next() error
}

// memIter yields, of each key, the version readers may see.
type memIter struct {
	n       *node
	visible uint64
	old     bool
}

func (l *skiplist) from(key []byte, visible uint64) *memIter {
	it := &memIter{n: l.seek(key, nil), visible: visible}
	it.skip()
	return it
}

func (it *memIter) skip() {
	for it.n != nil && it.n.r.at(it.visible) == nil {
		it.n = it.n.next[0]
	}
}

func (it *memIter) row() *row {
	if it.n == nil {
		return nil
	}
	if r := it.n.r.at(it.visible); !it.old {
		return r
	} else {
		return r.old
	}
}

func (it *memIter) next() error {
	if !it.old && it.n.r.at(it.visible).old != nil {
		it.old = true
		return nil
	}
	it.n, it.old = it.n.next[0], false
	it.skip()
	return nil
}
