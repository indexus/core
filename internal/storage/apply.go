package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"math"

	"github.com/indexus/go-indexus-core/internal/protocol"
)

func (s *Store) Write(requestID string, r Record) (Mutation, error) {
	mutation, _, err := s.Apply(requestID, r)
	return mutation, err
}

// Apply reports whether this call committed a new mutation, for pressure
// metering. Replayed requests must not be counted as newly accepted writes.
func (s *Store) Apply(requestID string, r Record) (Mutation, bool, error) {
	mutation, _, applied, err := s.ApplyRecord(requestID, r)
	return mutation, applied, err
}

// ApplyRecord also returns the record previously stored under the same ID when
// a new mutation commits. Derived summaries update by exact delta from it
// instead of rescanning the zone after every write.
func (s *Store) ApplyRecord(requestID string, r Record) (Mutation, *Record, bool, error) {
	return s.ApplyRecordContext(context.Background(), requestID, r)
}

// ApplyRecordContext observes cancellation before admission. After admission it
// returns the actual committed/refused result, even if the caller disconnects;
// retrying the same RequestID remains idempotent.
func (s *Store) ApplyRecordContext(ctx context.Context, requestID string, r Record) (Mutation, *Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Mutation{}, nil, false, err
	}
	w, err := newWriteRequest(requestID, r)
	if err != nil {
		return Mutation{}, nil, false, err
	}
	if s.fenceAdmission.contains(w.zone) {
		return Mutation{}, nil, false, ErrFrozen
	}
	// DESC-1: a record of a described collection carries one metric per
	// column of its schema.
	if c, err := s.CollectionOf(r.Collection); err != nil {
		return Mutation{}, nil, false, err
	} else if err := FitsCollection(c, r); err != nil {
		return Mutation{}, nil, false, err
	}
	// Cheap read of authority before taking the write lock: a frozen or
	// non-owned zone must not queue behind handoff disk work. The Batch path
	// still re-checks under the write lock.
	// It reads no row, so it does not wait for an update in flight.
	if err := s.peek(func(tx *Tx) error { return s.causalWritable(tx, w.zone) }); err != nil {
		return Mutation{}, nil, false, err
	}
	var out writeOutcome
	apply := func(tx *Tx) error {
		// The group committer may replay a callback when another one fails.
		// Reset caller-visible state before every execution.
		out = writeOutcome{}
		return s.applyWrite(tx, w, &out)
	}
	err = s.writes.submit(ctx, w.weight(), apply)
	if err == nil {
		err = out.refuse
	}
	if err != nil || !out.applied {
		out.previous = nil
	}
	return out.result, out.previous, out.applied && err == nil, err
}

// writeRequest is a write's immutable input, encoded once before acquiring
// the store's single writer. The same bytes serve as request identity and as the
// canonical stored record.
type writeRequest struct {
	zone       tree.ZoneKey
	requestID  string
	record     Record
	recordRaw  []byte
	digest     [32]byte
	zoneKey    []byte
	requestKey []byte // the key of its receipt
}

func newWriteRequest(requestID string, r Record) (*writeRequest, error) {
	z, err := recordZone(r)
	if err != nil {
		return nil, err
	}
	if requestID == "" {
		return nil, fmt.Errorf("mutation identity required")
	}
	if len(r.Metrics) > MaxMetrics {
		return nil, fmt.Errorf("%w: %d metrics, at most %d", ErrTooManyMetrics, len(r.Metrics), MaxMetrics)
	}
	if r.Weight < 0 || math.IsNaN(r.Weight) || math.IsInf(r.Weight, 0) {
		return nil, fmt.Errorf("record weight %v", r.Weight)
	}
	if r.Binary {
		var raw []byte
		if err := json.Unmarshal(r.Payload, &raw); err != nil || len(r.Payload) == 0 {
			return nil, fmt.Errorf("binary payload must be a base64 JSON string")
		}
	}
	if r.Weight == 1 {
		r.Weight = 0 // one form for one value: the JSON omits it
	}
	recordRaw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return &writeRequest{
		zone:       z,
		requestID:  requestID,
		record:     r,
		recordRaw:  recordRaw,
		digest:     sha256.Sum256(recordRaw),
		zoneKey:    zoneBytes(z),
		requestKey: []byte(requestID),
	}, nil
}

// weight includes encoded data, retained metric storage, identities and
// callback bookkeeping. It is an admission budget, not an RSS estimator.
func (w *writeRequest) weight() int64 {
	r := w.record
	return int64(len(w.recordRaw)+len(w.zoneKey)+len(w.requestKey)+len(r.Collection)+len(r.Location)+2*len(r.ID)) + int64(len(r.Metrics))*8 + 256
}

// writeOutcome is what one execution of the write callback reports to its caller.
type writeOutcome struct {
	result   Mutation
	previous *Record
	applied  bool
	// refuse is set when this write cannot proceed but must not fail its
	// group: returning an error from a callback aborts every other write
	// grouped in the same batch. Under a handoff freeze that means one frozen
	// zone stalls every concurrent write on the same disk, the open-loop
	// generator queue explodes, and the handoff itself starves for the write
	// lock. Soft-refuse keeps the batch commit for siblings and surfaces the
	// refusal only to this caller.
	refuse error
}

// applyWrite runs inside the batch transaction: authority, then replay, then
// the new mutation.
func (s *Store) applyWrite(tx *Tx, w *writeRequest, out *writeOutcome) error {
	// Recheck under writeMu: requests already queued before the preparation
	// gate appeared must not keep advancing the source's snapshot sequence.
	// Soft refusal preserves unrelated writes in the same group commit.
	if s.fenceAdmission.contains(w.zone) {
		out.refuse = ErrFrozen
		return nil
	}
	if err := s.causalWritable(tx, w.zone); err != nil {
		out.refuse = err
		return nil
	}
	if known, err := replayReceipt(tx, w, out); known || err != nil {
		return err
	}
	usage, err := s.storedDataUsage(tx)
	if err != nil {
		return err
	}
	totals := &writeTotals{usage: usage}
	zone := liveZone(w.zone)
	seq := zone.seq(tx)
	if seq == ^uint64(0) {
		return ErrSequence
	}
	newLiveZone := seq == 0 && tx.value(zone.row(sequenceKey)) == nil
	out.result = Mutation{w.requestID, seq + 1, w.record}
	if err := s.putLiveRecord(tx, zone, w, out, totals); err != nil {
		return err
	}
	if err := s.appendHistory(tx, w, out.result, &totals.usage); err != nil {
		return err
	}
	if err := putRequestReceipts(tx, zone, w, out.result, totals); err != nil {
		return err
	}
	if err := s.putWriteTotals(tx, totals); err != nil {
		return err
	}
	out.applied = true
	s.noteOwnedSeq(w.zone, out.result.Seq)
	if newLiveZone {
		return s.noteMigrationZone(tx, w.zone)
	}
	return nil
}

// writeTotals is the node's data usage, which one write updates by delta.
type writeTotals struct {
	usage protocol.ResourceCounters
}

// putLiveRecord replaces the record and advances the zone sequence, reporting
// the record it replaced so derived summaries can update by exact delta.
func (s *Store) putLiveRecord(tx *Tx, zone zoneTable, w *writeRequest, out *writeOutcome, totals *writeTotals) error {
	prior, delta, err := tx.putRecord(w.zone, w.record)
	if err != nil {
		return err
	}
	if prior != nil {
		previous := *prior.record
		out.previous = &previous
	}
	totals.usage.Owned += delta
	return tx.putCounted(zone.row(sequenceKey), sequenceKey, number(out.result.Seq), &totals.usage.Owned)
}

// rowDelta is what replacing a row of size old (-1: none) by one of size
// size changes in the usage counted against capacity: a new row also pays
// its key, 1+id bytes.
func rowDelta(id, old, size int) int64 {
	if old < 0 {
		return int64(1 + id + size)
	}
	return int64(size - old)
}

// appendHistory logs the mutation for a later handoff replay, unless the
// store was opened without history.
func (s *Store) appendHistory(tx *Tx, w *writeRequest, m Mutation, usage *protocol.ResourceCounters) error {
	if s.omitHistory {
		return nil
	}
	log := key(historyBucket, w.zoneKey)
	mutationRaw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return tx.putCounted(log+element(number(m.Seq)), number(m.Seq), mutationRaw, &usage.Owned)
}

// putWriteTotals commits the usage counter, enforces capacity on the whole
// transaction and counts the write as cold activity.
func (s *Store) putWriteTotals(tx *Tx, totals *writeTotals) error {
	if totals.usage.PhysicalLoad() > s.capacity {
		return ErrDataCapacity
	}
	if err := tx.putJSON(meta(usageKey), totals.usage); err != nil {
		return err
	}
	return noteColdWrite(tx)
}
