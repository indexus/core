package zonelog

import (
	"bytes"
	"context"
)

const childSegmentLimit = 8

// A zone's half-open bounds consist of just the fixed collection/location/depth
// head. For comparisons against those bounds, the variable-length document ID
// cannot change membership, including when a head equals either endpoint.
type childKeyRange struct{ first, last [headLen]byte }

type omittedChildRanges struct {
	ranges    []childKeyRange
	unbounded bool
}

// Called under the snapshot's store lock. Copy heads only: no segment page
// access or long-lived references to omitted segments and their document IDs.
func boundOmittedChildren(older []*segment) omittedChildRanges {
	if len(older) > childSegmentLimit {
		return omittedChildRanges{unbounded: true}
	}
	var result omittedChildRanges
	for _, g := range older {
		if g.pages == 0 {
			continue
		}
		if len(g.blocks) == 0 || len(g.blocks[0].first) < headLen || len(g.last) < headLen {
			return omittedChildRanges{unbounded: true}
		}
		if result.ranges == nil {
			result.ranges = make([]childKeyRange, 0, len(older))
		}
		result.ranges = append(result.ranges, childKeyRange{
			first: [headLen]byte(g.blocks[0].first), last: [headLen]byte(g.last),
		})
	}
	return result
}

func (r omittedChildRanges) mayOverlap(lo, hi []byte) bool {
	if r.unbounded {
		return true
	}
	for _, span := range r.ranges {
		if bytes.Compare(span.last[:], lo) >= 0 && bytes.Compare(span.first[:], hi) < 0 {
			return true
		}
	}
	return false
}

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

func (sn *Snapshot) observeChildPresence(ctx context.Context, collection, location string, omitted omittedChildRanges) (seen, unknown uint64, err error) {
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
		if status == childEmpty && omitted.mayOverlap(lo, hi) {
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
