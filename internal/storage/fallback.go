package storage

import (
	"encoding/json"
	"github.com/indexus/go-indexus-core/internal/tree"
	"strings"

	"github.com/indexus/go-indexus-core/internal/causal"
)

const (
	fallbackPrefix     = "fallback:"
	fallbackMetaPrefix = "fallback-meta:"
	fallbackCounterKey = "fallback-counter"
	fallbackVerKey     = "ver"
)

// FallbackEntry is one write accepted by a replica while the zone's owner was
// unreachable (convergent-replication.md §4-5).
type FallbackEntry struct {
	RequestID      string
	Record         Record
	DotIncarnation string
	DotCounter     uint64
	Ver            uint64
	Digest         [32]byte
}

type fallbackWinner struct {
	Ver            uint64
	DotIncarnation string
	DotCounter     uint64
	AppliedSeq     uint64
}

func fallbackSession(z tree.ZoneKey) string { return fallbackPrefix + sealKey(z) }

func fallbackTable(z tree.ZoneKey) zoneTable { return zoneTable(stagingRows(fallbackSession(z))) }

func fallbackMetaTable(z tree.ZoneKey) zoneTable {
	return zoneTable(stagingRows(fallbackMetaPrefix + sealKey(z)))
}

func isFallback(session string) bool {
	return strings.HasPrefix(session, fallbackPrefix) || strings.HasPrefix(session, fallbackMetaPrefix)
}

// ApplyFallback durably records a partition fallback write on a non-owner
// replica. It is idempotent by requestID, enforces maxFallbackBytes when > 0,
// and never makes the row visible to live reads, inventories, or summaries.
func (s *Store) ApplyFallback(requestID string, r Record, baseSeq uint64, maxFallbackBytes int64) (FallbackEntry, error) {
	if requestID == "" {
		return FallbackEntry{}, ErrConflict
	}
	z, err := recordZone(r)
	if err != nil {
		return FallbackEntry{}, err
	}
	id, err := s.Identity()
	if err != nil {
		return FallbackEntry{}, err
	}
	var entry FallbackEntry
	// s.update, not updateCausal: a client write is not a transfer step
	// (LastTransferStep).
	err = s.update(func(tx *Tx) error {
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		if receiverClosed(tx, state) {
			return ErrNotReady
		}
		cached, err := s.causalSnapshot(tx)
		if err != nil {
			return err
		}
		if err = s.writableIn(tx, cached, z); err == nil {
			return ErrConflict
		}

		t := fallbackTable(z)
		if raw := t.get(tx, receiptPrefix, []byte(requestID)); raw != nil {
			return json.Unmarshal(raw, &entry)
		}
		if maxFallbackBytes > 0 {
			var used int64
			if err := tx.each(string(t), "", func(_ string, v []byte) bool {
				used += int64(len(v))
				return used < maxFallbackBytes
			}); err != nil {
				return err
			}
			if used >= maxFallbackBytes {
				return ErrDataCapacity
			}
		}
		counter := tx.number8(meta([]byte(fallbackCounterKey))) + 1
		if err := tx.put(meta([]byte(fallbackCounterKey)), number(counter)); err != nil {
			return err
		}
		ver := max(baseSeq, tx.number8(t.row([]byte(fallbackVerKey)))) + 1
		if err := tx.put(t.row([]byte(fallbackVerKey)), number(ver)); err != nil {
			return err
		}
		entry = FallbackEntry{
			RequestID:      requestID,
			Record:         r,
			DotIncarnation: id,
			DotCounter:     counter,
			Ver:            ver,
			Digest:         digest(r),
		}
		if err := t.putJSON(tx, recordPrefix, []byte(r.ID), entry); err != nil {
			return err
		}
		if err := t.putJSON(tx, receiptPrefix, []byte(requestID), entry); err != nil {
			return err
		}
		return tx.put(stagingRows(fallbackSession(z)), nil)
	})
	return entry, err
}

// FallbackZones returns every zone that currently has a pending fallback
// session on this disk.
func (s *Store) FallbackZones() ([]tree.ZoneKey, error) {
	var out []tree.ZoneKey
	err := s.view(func(tx *Tx) error {
		sessions, err := stagingSessions(tx)
		if err != nil {
			return err
		}
		for _, session := range sessions {
			if !strings.HasPrefix(session, fallbackPrefix) {
				continue
			}
			col, loc, ok := strings.Cut(strings.TrimPrefix(session, fallbackPrefix), "\x00")
			if !ok {
				continue
			}
			z, err := tree.NewZoneKey(col, loc)
			if err != nil {
				continue
			}
			out = append(out, z)
		}
		return nil
	})
	return out, err
}

// FallbackEntries returns all pending fallback writes for z in deterministic key order.
func (s *Store) FallbackEntries(z tree.ZoneKey) ([]FallbackEntry, error) {
	t := fallbackTable(z)
	var out []FallbackEntry
	err := s.view(func(tx *Tx) error {
		return t.each(tx, recordPrefix, "", func(_, raw []byte) error {
			var e FallbackEntry
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			out = append(out, e)
			return nil
		})
	})
	return out, err
}

// DegradedRead returns a record from the replica's fallback log (if written
// during a partition) or else from owner's shadow copy of z. It never reads
// authoritative live data, and returns ok=false when the winning record is
// absent or deleted (SHADOW-1, READ-1 degraded path).
func (s *Store) DegradedRead(owner causal.Endpoint, z tree.ZoneKey, id string) (Record, bool, error) {
	txName, err := s.validShadowOwner(owner)
	if err != nil {
		return Record{}, false, err
	}
	var out Record
	var ok, foundFallback bool
	err = s.view(func(tx *Tx) error {
		if raw := fallbackTable(z).get(tx, recordPrefix, []byte(id)); raw != nil {
			var e FallbackEntry
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			foundFallback = true
			if !e.Record.Deleted {
				out, ok = e.Record, true
			}
		}
		return nil
	})
	if err != nil || foundFallback {
		return out, ok, err
	}
	err = s.shadowView(func(tx *Tx) error {
		t, header, err := stagedZone(tx, txName, z)
		if err == ErrNotReady {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Source != owner.Storage {
			return nil
		}
		if raw := t.get(tx, recordPrefix, []byte(id)); raw != nil {
			var r Record
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			if !r.Deleted {
				out, ok = r, true
			}
		}
		return nil
	})
	return out, ok, err
}

// AckFallback removes fallback entries confirmed by the zone's owner.
func (s *Store) AckFallback(z tree.ZoneKey, requestIDs []string) error {
	if len(requestIDs) == 0 {
		return nil
	}
	t := fallbackTable(z)
	want := make(map[string]struct{}, len(requestIDs))
	for _, id := range requestIDs {
		want[id] = struct{}{}
	}
	return s.update(func(tx *Tx) error {
		var recordKeys [][]byte
		if err := t.each(tx, recordPrefix, "", func(k, raw []byte) error {
			var e FallbackEntry
			if json.Unmarshal(raw, &e) == nil {
				if _, ok := want[e.RequestID]; ok {
					recordKeys = append(recordKeys, append([]byte(nil), k...))
				}
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range recordKeys {
			if err := tx.del(t.entry(recordPrefix, k)); err != nil {
				return err
			}
		}
		for _, id := range requestIDs {
			if err := tx.del(t.entry(receiptPrefix, []byte(id))); err != nil {
				return err
			}
		}
		var hasPending bool
		if err := t.each(tx, recordPrefix, "", func(_, _ []byte) error {
			hasPending = true
			return nil
		}); err != nil {
			return err
		}
		if !hasPending {
			if err := t.each(tx, receiptPrefix, "", func(_, _ []byte) error {
				hasPending = true
				return nil
			}); err != nil {
				return err
			}
		}
		if !hasPending {
			return tx.drop(stagingRows(fallbackSession(z)))
		}
		return nil
	})
}

func historyRecordSeqs(tx *Tx, z tree.ZoneKey) map[string]uint64 {
	seqs := map[string]uint64{}
	log := key(historyBucket, zoneBytes(z))
	_ = tx.each(log, "", func(_ string, raw []byte) bool {
		var m Mutation
		if json.Unmarshal(raw, &m) == nil && m.Seq > seqs[m.Record.ID] {
			seqs[m.Record.ID] = m.Seq
		}
		return true
	})
	return seqs
}

// MergeFallback repairs a zone on its owner by applying winning fallback
// entries through the normal authoritative write path in one transaction.
func (s *Store) MergeFallback(z tree.ZoneKey, entries []FallbackEntry, wins func(a, b FallbackEntry) bool) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	merge := fallbackMerge{store: s, zone: z, wins: wins}
	err := s.update(func(tx *Tx) error {
		if err := merge.open(tx); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := merge.apply(entry); err != nil {
				return err
			}
		}
		return nil
	})
	return merge.applied, err
}

// fallbackMerge applies fallback entries to one zone inside one transaction.
type fallbackMerge struct {
	store *Store
	zone  tree.ZoneKey
	wins  func(a, b FallbackEntry) bool

	tx       *Tx
	live     map[string]Record // the zone's records, as the merge leaves them
	zoneSeq  uint64
	histSeqs map[string]uint64 // each record's seq in the history, read on first need
	applied  int
}

func (m *fallbackMerge) open(tx *Tx) error {
	if err := m.store.causalWritable(tx, m.zone); err != nil {
		return err
	}
	rows, err := rowsOf(tx, m.zone, true)
	if err != nil {
		return err
	}
	m.tx, m.live, m.zoneSeq = tx, map[string]Record{}, liveZone(m.zone).seq(tx)
	for _, row := range rows {
		if row.record != nil {
			m.live[row.record.ID] = *row.record
		}
	}
	return nil
}

// apply writes one entry unless the record the zone holds wins over it, and
// remembers the version of what it wrote.
func (m *fallbackMerge) apply(entry FallbackEntry) error {
	w, err := newWriteRequest(entry.RequestID, entry.Record)
	if err != nil {
		return err
	}
	if w.zone != m.zone {
		return ErrConflict
	}
	id := entry.Record.ID
	if cur, exists := m.live[id]; exists && m.wins != nil && !m.wins(entry, m.current(cur)) {
		return nil
	}
	var out writeOutcome
	if err := m.store.applyWrite(m.tx, w, &out); err != nil {
		return err
	}
	if out.refuse != nil {
		return out.refuse
	}
	if !out.applied {
		return nil
	}
	m.applied++
	m.live[id] = entry.Record
	if m.histSeqs != nil {
		m.histSeqs[id] = out.result.Seq
	}
	winner := fallbackWinner{Ver: entry.Ver, DotIncarnation: entry.DotIncarnation, DotCounter: entry.DotCounter, AppliedSeq: out.result.Seq}
	return fallbackMetaTable(m.zone).putJSON(m.tx, recordPrefix, []byte(id), winner)
}

// current is the record the zone holds, as a fallback entry: with the
// version of the fallback write that produced it when it is still that
// write, otherwise with its seq, and never below a version already merged.
func (m *fallbackMerge) current(cur Record) FallbackEntry {
	if m.histSeqs == nil {
		m.histSeqs = historyRecordSeqs(m.tx, m.zone)
	}
	seq := m.histSeqs[cur.ID]
	if seq == 0 {
		seq = m.zoneSeq
	}
	entry := FallbackEntry{Record: cur, Ver: seq, Digest: digest(cur)}
	raw := fallbackMetaTable(m.zone).get(m.tx, recordPrefix, []byte(cur.ID))
	if raw == nil {
		return entry
	}
	var prev fallbackWinner
	if json.Unmarshal(raw, &prev) == nil && seq <= prev.AppliedSeq {
		entry.Ver, entry.DotIncarnation, entry.DotCounter = prev.Ver, prev.DotIncarnation, prev.DotCounter
	} else if prev.Ver > entry.Ver {
		entry.Ver = prev.Ver
	}
	return entry
}
