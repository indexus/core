package storage

import (
	"encoding/binary"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// The staging index (2026-11-26). Capacity accounting needs the sessions
// that have staged rows (reservationDelta). It found them by seeking through
// the staging rows, one seek per session, and a seek walks every deleted row
// in its way: the rows of the sessions already collected stay in zonelog as
// tombstones until a merge drops them. On AWS on 2026-11-26 that walk was
// 33 % of a receiver's CPU, under the writer, and grew with every handoff
// the receiver had finished (docs/benchmarks/receiver-writer-20261126.md).
//
// The index is one row per session that has at least one staged row, the
// reservation row staging/<session> included: staging-sessions/<session> →
// the number of its rows. Every update that writes or deletes a staged row
// commits through Tx.commit, which brings the index along in the same
// zonelog batch (indexStaging): the index is exact after every commit and
// after a crash, whoever the writer is (preload, mirror, mount, install,
// collect, fallback). A row is about content, not about open transfers: an
// installed transfer keeps its row until compactStaging deletes its last
// staged row. Index rows are not counted in the usage.
//
// The index rows of collected sessions are tombstones in their turn, one
// per session and no longer one per staged row. The writer does not walk
// them either: the store keeps the indexed sessions in memory (Store.staged),
// read from the index at Open and changed with each commit.
//
// The shadow store (shadow.go) is another zonelog store, with no capacity
// accounting: it keeps no index.

func stagingIndexRow(session string) string { return key(stagingIndexBucket, []byte(session)) }

// firstElement is the first element of a path.
func firstElement(path string) string {
	i := 0
	for i+1 < len(path) && !(path[i] == 0 && path[i+1] == 0) {
		if path[i] == 0 {
			i++ // an escaped 0x00
		}
		i++
	}
	return string(unescape(path[:min(i+2, len(path))]))
}

// stagedDelta is what this update changes in the number of staged rows of
// each session it touches: one point read per changed row, remembered
// (committedSize), and no scan.
func stagedDelta(tx *Tx) map[string]int64 {
	staging := key(stagingBucket)
	delta := map[string]int64{}
	for path, payload := range tx.pending {
		if path == staging || !strings.HasPrefix(path, staging) {
			continue
		}
		var d int64
		if _, existed := tx.committedSize(path); existed {
			d--
		}
		if payload != nil {
			d++
		}
		delta[firstElement(path[len(staging):])] += d
	}
	return delta
}

// indexStaging writes what the index is once this update commits: for each
// session the update touches, the committed count plus the update's delta,
// and no row at zero. Always computed from the committed row, it can run
// several times in one update (a capacity check, then the commit). It keeps
// in tx.staged whether each touched session still has a staged row.
func (tx *Tx) indexStaging() error {
	if tx.owner == nil || !tx.writable {
		return nil
	}
	tx.copied = copiedDelta(tx)
	for session, d := range stagedDelta(tx) {
		row := stagingIndexRow(session)
		var count int64
		if raw, ok := committedValue(tx, row); ok && len(raw) == 8 {
			count = int64(binary.BigEndian.Uint64(raw))
		}
		count += d
		var err error
		switch {
		case count < 0:
			return fmt.Errorf("storage: staging index of session %q counts %d rows", session, count)
		case uint64(count) == tx.number8(row):
		case count == 0:
			err = tx.del(row)
		default:
			err = tx.put(row, number(uint64(count)))
		}
		if err != nil {
			return err
		}
		if tx.staged == nil {
			tx.staged = map[string]bool{}
		}
		tx.staged[session] = count > 0
	}
	return nil
}

// stagingSessions are the sessions with a staged row after this update, in
// order. The writer takes the ones of the last commit from memory
// (Store.staged) and applies what this update changes: no scan at all, so
// not even the index rows of collected sessions, tombstones too until a
// merge, are in its way. Any other transaction (a view, a preparation on a
// snapshot) reads the index rows of its own version.
func stagingSessions(tx *Tx) ([]string, error) {
	if err := tx.indexStaging(); err != nil {
		return nil, err
	}
	if !tx.live() {
		return indexedSessions(tx)
	}
	out := make([]string, 0, len(tx.owner.staged)+len(tx.staged))
	for _, session := range tx.owner.staged {
		if left, touched := tx.staged[session]; left || !touched {
			out = append(out, session)
		}
	}
	for session, left := range tx.staged {
		if _, held := slices.BinarySearch(tx.owner.staged, session); left && !held {
			out = append(out, session)
		}
	}
	slices.Sort(out)
	if !verifyUsage {
		return out, nil
	}
	// Every test is a differential test of the sessions kept in memory.
	indexed, err := indexedSessions(tx)
	if err == nil && !slices.Equal(indexed, out) {
		err = fmt.Errorf("storage: staging sessions in memory %q, indexed %q", out, indexed)
	}
	return out, err
}

// live is whether tx is an update of its store reading the last commit: the
// writer, under writeMu. A preparation writes on a snapshot and is not.
func (tx *Tx) live() bool {
	return tx.owner != nil && tx.writable && tx.ownReader.Reader == zonelog.Reader(tx.db)
}

// indexedSessions reads the index rows as tx sees them.
func indexedSessions(tx *Tx) ([]string, error) {
	index := key(stagingIndexBucket)
	var out []string
	err := tx.scanRows(index, "", false, func(path string, _ []byte) bool {
		if path != index {
			out = append(out, firstElement(path[len(index):]))
		}
		return true
	})
	return out, err
}

// noteStaged brings Store.staged to what the update just committed. The
// caller holds writeMu.
func (s *Store) noteStaged(tx *Tx) {
	if !tx.live() {
		return
	}
	for session, left := range tx.staged {
		at, held := slices.BinarySearch(s.staged, session)
		switch {
		case left && !held:
			s.staged = slices.Insert(slices.Clone(s.staged), at, session)
		case !left && held:
			s.staged = slices.Delete(slices.Clone(s.staged), at, at+1)
		}
	}
}

// loadStaged reads the index into memory when the store opens.
func (s *Store) loadStaged() error {
	return s.view(func(tx *Tx) (err error) {
		s.staged, err = indexedSessions(tx)
		return err
	})
}

// backfillStagingIndex builds the index of a store written by a build that
// kept none: it has staged rows and no index row. One scan of the staging
// rows, once, in the update that opens the store; the row it leaves at the
// index itself (staging-sessions) makes every later open skip it. A store
// this build wrote and an older build then wrote to is not detected.
func backfillStagingIndex(tx *Tx) error {
	index := key(stagingIndexBucket)
	if tx.value(index) != nil {
		return nil
	}
	staging := key(stagingBucket)
	counts := map[string]uint64{}
	if err := tx.scanRows(staging, "", false, func(path string, _ []byte) bool {
		if path != staging {
			counts[firstElement(path[len(staging):])]++
		}
		return true
	}); err != nil {
		return err
	}
	for session, n := range counts {
		if err := tx.put(stagingIndexRow(session), number(n)); err != nil {
			return err
		}
	}
	return tx.put(index, []byte{1})
}

// checkStagingIndex compares the index with the staged rows, as tx sees
// both: a session's row counts exactly its staged rows, and a session
// without staged rows has no row. One scan of the staging rows: for tests,
// where every commit runs it (verifyUsage), and for assertStagingIndex.
func checkStagingIndex(tx *Tx) error {
	staging, index := key(stagingBucket), key(stagingIndexBucket)
	rows, indexed := map[string]uint64{}, map[string]uint64{}
	if err := tx.scanRows(staging, "", false, func(path string, _ []byte) bool {
		if path != staging {
			rows[firstElement(path[len(staging):])]++
		}
		return true
	}); err != nil {
		return err
	}
	if err := tx.each(index, "", func(path string, value []byte) bool {
		if len(value) == 8 {
			indexed[firstElement(path[len(index):])] = binary.BigEndian.Uint64(value)
		}
		return true
	}); err != nil {
		return err
	}
	if !maps.Equal(rows, indexed) {
		return fmt.Errorf("storage: staged rows by session %v, staging index %v", rows, indexed)
	}
	return nil
}
