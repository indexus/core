package zonelog

import (
	"bytes"
	"context"
	"errors"
)

// ErrContentScanLimit means the complete local subtree was not observed.
var ErrContentScanLimit = errors.New("bounded content scan unavailable")

// ContentSnapshot declines a cold scan when opening all sources would exceed
// its budget. Only the existing bounded prefix metadata is examined on refusal.
func (s *Store) ContentSnapshot() (*Snapshot, bool) {
	sn, omitted := s.pinSnapshot(32)
	if omitted.unbounded || len(omitted.ranges) != 0 {
		sn.Close()
		return nil, false
	}
	return sn, true
}

type contentBudget struct {
	ctx  context.Context
	rows int
}

func (b *contentBudget) charge() error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if b.rows == 0 {
		return ErrContentScanLimit
	}
	b.rows--
	return nil
}

type limitedContentIter struct {
	iter
	budget *contentBudget
}

func (it limitedContentIter) next() error {
	if err := it.budget.charge(); err != nil {
		return err
	}
	return it.iter.next()
}

// Copy immutable row references under the lock. A post-snapshot version makes
// the probe inconclusive instead of following an unbounded MVCC chain.
func (sn *Snapshot) contentMem(mem *skiplist, lo, hi []byte, budget *contentBudget) (iter, error) {
	it := &memChunkIter{done: true}
	if mem == nil {
		return it, nil
	}
	sn.s.mu.RLock()
	defer sn.s.mu.RUnlock()
	for n := mem.seek(lo, nil); n != nil && bytes.Compare(n.r.key, hi) < 0; n = n.next[0] {
		if err := budget.charge(); err != nil {
			return nil, err
		}
		if n.r.seq > sn.seq {
			return nil, ErrContentScanLimit
		}
		it.buf = append(it.buf, n.r)
		if n.r.old != nil {
			it.buf = append(it.buf, n.r.old)
		}
	}
	return it, nil
}

// ScanContent bounds both physical iterator steps (including obsolete keys)
// and the logical bytes loaded. Row references from disk remain valid while
// the snapshot is pinned. The visitor receives the same visible newest row
// chosen by ordinary snapshot scans.
func (sn *Snapshot) ScanContent(ctx context.Context, collection, location string, maxSteps, maxBytes int, visit func(Record) error) error {
	sn.s.mu.RLock()
	col, ok := sn.s.cols[collection]
	sn.s.mu.RUnlock()
	if !ok || int(col) >= sn.cols {
		return ctx.Err()
	}
	lo, hi, err := zoneRange(col, location)
	if err != nil {
		return err
	}
	budget := contentBudget{ctx, maxSteps}
	var sources []iter
	for _, mem := range []*skiplist{sn.mem, sn.imm} {
		it, e := sn.contentMem(mem, lo, hi, &budget)
		if e != nil {
			return e
		}
		sources = append(sources, limitedContentIter{it, &budget})
	}
	for i := len(sn.segs) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		g := sn.segs[i]
		if g.pages == 0 || bytes.Compare(g.last, lo) < 0 || bytes.Compare(g.blocks[0].first, hi) >= 0 {
			continue
		}
		at, e := g.seek(sn.s.cache, lo)
		if e != nil {
			return e
		}
		it, e := g.from(sn.s.cache, at)
		if e != nil {
			return e
		}
		sources = append(sources, limitedContentIter{it, &budget})
	}
	var keys, dead int64
	defer func() { scanKeys.Add(keys); scanDead.Add(dead) }()
	return eachKey(sources, hi, func(real, _ []*row) (bool, error) {
		keys++
		if len(real) == 0 || !real[0].live() || real[0].flags&flagUncounted != 0 {
			dead++
			return true, nil
		}
		// Only the value fields are immutable in a memtable row. A writer
		// can detach its old MVCC chain while this scan is hashing.
		source := real[0]
		r := row{key: source.key, flags: source.flags, weight: source.weight,
			metrics: source.metrics, payload: source.payload,
			val: source.val, voff: source.voff, vlen: source.vlen}
		size := len(r.payload)
		if r.payload == nil {
			size = int(r.vlen)
		}
		cost := len(r.key) + 8*(1+len(r.metrics)) + size
		if cost > maxBytes {
			return false, ErrContentScanLimit
		}
		maxBytes -= cost
		if e := sn.s.payload(&r); e != nil {
			return false, e
		}
		e := visit(recordWithCollection(collection, &r))
		return e == nil, e
	})
}
