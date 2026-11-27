package zonelog

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
)

// mutation owns its input slices; it performs no store access.
func mutation(rec Record, col uint16) (*row, error) {
	key, err := makeKey(col, rec.Location, rec.ID)
	if err != nil || len(rec.Metrics) > MaxMetrics {
		return nil, errors.Join(err, fmt.Errorf("zonelog: invalid mutation for %q", rec.ID))
	}
	// The store keeps its own copy: a caller may reuse its slices.
	r := &row{key: key, weight: rec.Weight, metrics: slices.Clone(rec.Metrics), payload: bytes.Clone(rec.Payload)}
	switch {
	case rec.Deleted:
		r.flags, r.weight, r.metrics, r.payload = flagDeleted, 0, nil, nil
	case r.weight == 0:
		r.weight = 1
	}
	if !rec.Deleted {
		if rec.Uncounted {
			r.flags, r.weight, r.metrics = flagUncounted, 0, nil
		}
		if rec.Opaque {
			r.flags |= flagOpaque
		}
		if rec.Binary {
			r.flags |= flagBinary
		}
	}
	return r, nil
}

// prepareSnapshot is one optimistic preparation, never a retry loop. Existing
// storage writers serialize transactions above this layer; concurrent raw
// Apply/Bulk callers retain the locked validation path when the seq changes.
func (s *Store) prepareSnapshot(snapshot *Snapshot, batch []Record) ([]*row, []*row, bool, error) {
	s.mu.RLock()
	if s.seq != snapshot.seq || s.bulk || s.err != nil {
		s.mu.RUnlock()
		return nil, nil, false, nil
	}
	columns := make([]uint16, len(batch))
	for i, rec := range batch {
		col, ok := s.cols[rec.Collection]
		if !ok {
			s.mu.RUnlock()
			return nil, nil, false, nil
		}
		columns[i] = col
	}
	s.mu.RUnlock()
	rows, prevs := make([]*row, len(batch)), make([]*row, len(batch))
	for i, rec := range batch {
		r, err := mutation(rec, columns[i])
		if err != nil {
			return nil, nil, false, err
		}
		prev, err := snapshot.lookup(rec.Collection, rec.Location, rec.ID)
		if err != nil {
			return nil, nil, false, err
		}
		same, err := s.unchanged(prev, r)
		if err != nil {
			return nil, nil, false, err
		}
		if !same {
			rows[i], prevs[i] = r, prev
		}
	}
	return rows, prevs, true, nil
}
