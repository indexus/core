package storage

import (
	"fmt"
	"maps"
	"strings"
)

// Staged copies by request (2026-11-26). What a request reserves depends on
// its staged copies in every session, and capacity accounting looked for
// them with one read per session and per request: a collection or a preload
// of R receipts on a receiver with S staging sessions read R x S rows under
// the writer (Codex's measurement 45eb506: 136, 584, 2120 reads for 64
// receipts with 1, 8, 32 sessions).
//
// The store keeps in memory how many staged copies each request has at the
// last commit (Store.copies). When every copy of a request is in a row the
// update changes, no other session can hold one, and only the sessions the
// update stages in are read. It is counted from the staged rows when the
// store opens (a store that was not opened keeps none and reads every
// session) and changed with each commit, like Store.staged; nothing is
// stored, so a crash loses nothing. Its size is the receipts in staging.

// stagedRequest is the request of a staged receipt row, given the path
// after the staging prefix.
func stagedRequest(rest string) (string, bool) {
	elems := split(rest)
	if len(elems) != 3 || len(elems[2]) == 0 || elems[2][0] != receiptPrefix {
		return "", false
	}
	return string(elems[2][1:]), true
}

// copiedDelta is what this update changes in the staged copies of each
// request: one remembered point read per changed receipt row.
func copiedDelta(tx *Tx) map[string]int32 {
	staging := key(stagingBucket)
	delta := map[string]int32{}
	for path, payload := range tx.pending {
		if path == staging || !strings.HasPrefix(path, staging) {
			continue
		}
		request, ok := stagedRequest(path[len(staging):])
		if !ok {
			continue
		}
		if _, existed := tx.committedSize(path); existed {
			delta[request]--
		}
		if payload != nil {
			delta[request]++
		}
	}
	return delta
}

// noteCopies brings Store.copies to what the update just committed. The
// caller holds writeMu.
func (s *Store) noteCopies(tx *Tx) {
	if !tx.live() || s.copies == nil {
		return
	}
	for request, d := range tx.copied {
		if n := s.copies[request] + d; n > 0 {
			s.copies[request] = n
		} else {
			delete(s.copies, request)
		}
	}
}

// loadCopies counts the staged copies when the store opens: one scan of the
// staged rows, without their values, and none when no session has any.
func (s *Store) loadCopies() error {
	if len(s.staged) == 0 {
		s.copies = map[string]int32{}
		return nil
	}
	return s.view(func(tx *Tx) (err error) {
		s.copies, err = stagedCopies(tx)
		return err
	})
}

// stagedCopies counts the staged copies of each request as tx sees the rows.
func stagedCopies(tx *Tx) (map[string]int32, error) {
	staging := key(stagingBucket)
	copies := map[string]int32{}
	err := tx.scanRows(staging, "", false, func(path string, _ []byte) bool {
		if request, ok := stagedRequest(path[len(staging):]); ok && path != staging {
			copies[request]++
		}
		return true
	})
	return copies, err
}

// checkCopies compares what the copies in memory become with this update
// and the staged rows as it leaves them. One scan: for tests, where every
// commit runs it.
func checkCopies(tx *Tx) error {
	if tx.owner.copies == nil {
		return nil
	}
	rows, err := stagedCopies(tx)
	if err != nil {
		return err
	}
	kept := maps.Clone(tx.owner.copies)
	for request, d := range tx.copied {
		if kept[request] += d; kept[request] == 0 {
			delete(kept, request)
		}
	}
	if !maps.Equal(rows, kept) {
		return fmt.Errorf("storage: %d requests have staged copies, %d in memory", len(rows), len(kept))
	}
	return nil
}
