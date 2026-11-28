package zonelog

import (
	"bytes"
	"context"
)

const childSegmentLimit = 8

type childObservation byte

const (
	childEmpty childObservation = iota
	childSeen
	childUnknown
)

// ObserveChildPresence samples each immediate child range on this disk.
// The routing alphabet indexes the bits. Seen means a counted local witness;
// unknown means an unusable physical key or omitted older segments.
// The parent itself is excluded. No payload is loaded.
func (s *Store) ObserveChildPresence(ctx context.Context, collection, location string) (uint64, uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	sn, omitted := s.pinSnapshot(childSegmentLimit)
	defer sn.Close()
	return sn.observeChildPresence(ctx, collection, location, omitted)
}

func (sn *Snapshot) observeChildPresence(ctx context.Context, collection, location string, omitted bool) (seen, unknown uint64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	_, depth, err := locBits(location)
	if err != nil || depth == locLen*8/6 {
		return 0, 0, err
	}
	if depth == 0 {
		location = ""
	}
	sn.s.mu.RLock()
	col, ok := sn.s.cols[collection]
	sn.s.mu.RUnlock()
	if !ok || int(col) >= sn.cols {
		return 0, 0, nil
	}
	for i := range len(alphabet) {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		lo, hi, err := zoneRange(col, location+alphabet[i:i+1])
		if err != nil {
			return 0, 0, err
		}
		status, err := sn.firstChildWitness(ctx, lo, hi)
		if err != nil {
			return 0, 0, err
		}
		if status == childEmpty && omitted {
			status = childUnknown
		}
		switch status {
		case childSeen:
			seen |= uint64(1) << i
		case childUnknown:
			unknown |= uint64(1) << i
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	return seen, unknown, nil
}

func (sn *Snapshot) firstInMemtable(table *skiplist, lo, hi []byte) childObservation {
	if table == nil {
		return childEmpty
	}
	n := table.seek(lo, nil)
	if n == nil || bytes.Compare(n.r.key, hi) >= 0 {
		return childEmpty
	}
	// A write after the snapshot can obscure an older version. Stop at the
	// current physical key instead of traversing its version chain.
	if n.r.seq > sn.seq || n.r.flags&^payloadFlags != 0 {
		return childUnknown
	}
	return childSeen
}

func (sn *Snapshot) firstChildWitness(ctx context.Context, lo, hi []byte) (childObservation, error) {
	sn.s.mu.RLock()
	status := sn.firstInMemtable(sn.mem, lo, hi)
	sn.s.mu.RUnlock()
	if status != childEmpty {
		return status, nil
	}
	if status = sn.firstInMemtable(sn.imm, lo, hi); status != childEmpty {
		return status, nil
	}
	// Newest sources first. Once a source contains a key in the range,
	// older sources cannot invalidate its witness. A tombstone can hide
	// a later live key, so it proves neither presence nor absence.
	for i := len(sn.segs) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return childEmpty, err
		}
		g := sn.segs[i]
		if g.pages == 0 || bytes.Compare(g.last, lo) < 0 || bytes.Compare(g.blocks[0].first, hi) >= 0 {
			continue
		}
		at, err := g.seek(sn.s.cache, lo)
		if err != nil {
			return childEmpty, err
		}
		if at.page >= g.pages {
			continue
		}
		p, err := sn.s.cache.page(g, at.page)
		if err != nil {
			return childEmpty, err
		}
		if bytes.Compare(p.head(at.row), hi) >= 0 {
			continue
		}
		if p.raw[p.flags+at.row]&^payloadFlags != 0 {
			return childUnknown, nil
		}
		return childSeen, nil
	}
	return childEmpty, nil
}
