package storage

import (
	"encoding/binary"
	"math"

	"github.com/indexus/go-indexus-core/internal/causal"
)

var summaryEpochKey = []byte("summary-epoch-v1")

// NextSummaryEpoch commits a new incarnation of the derived summary runtime.
// It is local to this durable store, never a replacement for its StorageID.
// A failed commit exposes no epoch; malformed counters and overflow fail closed.
func (s *Store) NextSummaryEpoch() (uint64, error) {
	var epoch uint64
	err := s.update(func(tx *Tx) error {
		var err error
		epoch, err = advanceSummaryEpoch(tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

func advanceSummaryEpoch(tx *Tx) (uint64, error) {
	raw := tx.value(meta(summaryEpochKey))
	var previous uint64
	if raw != nil {
		if len(raw) != 8 {
			return 0, causal.ErrConflict
		}
		previous = binary.BigEndian.Uint64(raw)
	}
	if previous == math.MaxUint64 {
		return 0, causal.ErrConflict
	}
	next := previous + 1
	if err := tx.put(meta(summaryEpochKey), number(next)); err != nil {
		return 0, err
	}
	return next, nil
}
