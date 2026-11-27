package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/internal/protocol"
)

// Incremental capacity accounting (REM-87). dataUsage scans every counted
// row: called after each batch of a handoff, it made a receiver pay for all
// the data it already held, a third to two thirds of its CPU during a split.
// The stored usage is exact after every commit (assertStoredUsage), so the
// usage after this update is the committed usage plus what the update's own
// rows change: O(rows changed), the same numbers as the scan.
//
// The scan counts a bucket as bbolt did: each row's last element and value,
// and each path element once. Equivalently, for the rows below a bucket:
//
//	size = Σ rows (len(last element) + len(value))
//	     + Σ paths that have a row below and are not a row (len(last element))
//
// so an update changes it only at its rows and at their ancestors.

// usageAfter is the usage once this update commits. ok is false when there
// is no stored usage to start from.
func (s *Store) usageAfter(tx *Tx) (used protocol.ResourceCounters, ok bool, err error) {
	raw, exists := committedValue(tx, meta(usageKey))
	if !exists {
		return used, false, nil
	}
	if err := json.Unmarshal(raw, &used); err != nil {
		return used, false, err
	}
	used.Owned += tx.delta // record rows
	paths := sortedKeys(tx.pending)
	for _, name := range [][]byte{liveBucket, historyBucket, operationsBucket, stagingBucket, transferBucket} {
		d := bucketDelta(tx, key(name), paths)
		if bytes.Equal(name, stagingBucket) || bytes.Equal(name, transferBucket) {
			used.Staging += d
		} else {
			used.Owned += d
		}
	}
	d, err := reservationDelta(tx, paths)
	used.ReservedIncoming += d
	return used, true, err
}

// committedValue is the value at path before this update.
func committedValue(tx *Tx, path string) ([]byte, bool) {
	r, ok, err := tx.reader.Get(kvCollection, "@", path)
	if err != nil || !ok || len(r.Payload) == 0 {
		return nil, false
	}
	return r.Payload[1:], true
}

// pendingValue is the value at path after this update.
func pendingValue(tx *Tx, path string) ([]byte, bool) {
	p, err := tx.get(path)
	if err != nil || len(p) == 0 {
		return nil, false
	}
	return p[1:], true
}

// lastElement is the length of the last element of path below prefix.
func lastElement(prefix, path string) int64 {
	elems := split(path[len(prefix):])
	return int64(len(elems[len(elems)-1]))
}

// bucketDelta is what the update changes in the rows below prefix: each row
// counts its last element and its value (Tx.size). One point read per
// changed row; no scan.
func bucketDelta(tx *Tx, prefix string, paths []string) int64 {
	var d int64
	for _, p := range paths {
		if p == prefix || !strings.HasPrefix(p, prefix) {
			continue
		}
		last := lastElement(prefix, p)
		if size, ok := tx.committedSize(p); ok {
			d -= last + int64(max(0, size-1)) // the stored payload starts with 'v'
		}
		if v, ok := pendingValue(tx, p); ok {
			d += last + int64(len(v))
		}
	}
	return d
}

// reservationDelta is what the update changes in ReservedIncoming: the
// receipts staged for a zone whose request is not yet in the node-wide
// ledger (operations), each request counted once. A request's receipt names
// its zone, so the staged copies of one request are that zone's in each
// staging session: point reads, not a scan. Conflicting copies are refused as
// the scan refuses them.
func reservationDelta(tx *Tx, paths []string) (int64, error) {
	affected, sessions, err := affectedRequests(tx, paths)
	if err != nil || len(affected) == 0 {
		return 0, err
	}
	// A row this update does not change is the same before and after: it is
	// read once (2026-11-26: half the point reads of a preload were the
	// second read of such a row).
	read := map[string][]byte{}
	before := func(path string) ([]byte, bool) {
		v, known := read[path]
		if !known {
			v, _ = committedValue(tx, path)
			read[path] = v
		}
		return v, v != nil
	}
	after := func(path string) ([]byte, bool) {
		if _, changed := tx.pending[path]; changed {
			return pendingValue(tx, path)
		}
		return before(path)
	}
	// The staging prefixes to look for copies under, in session order:
	// every session, or only those the update stages in.
	var all, touched []string
	var d int64
	for request, r := range affected {
		ledger := key(operationsBucket, []byte(request))
		if _, written := tx.pending[ledger]; !written && !r.staged {
			// The update only drops staged copies of a request the ledger
			// already holds, unchanged: it reserved nothing and reserves
			// nothing, whatever the sessions hold. A collection is made of
			// these (AWS 2026-11-26: a receiver's writer held 9.85 s of 10
			// reading, for each collected receipt, every staging session).
			if _, held := before(ledger); held {
				continue
			}
		}
		under := &all
		if tx.live() && tx.owner.copies != nil && int(tx.owner.copies[request]) == r.changed {
			// Every staged copy of the request is in a row this update
			// changes: no other session holds one (staging_copies.go).
			under = &touched
		}
		if *under == nil {
			if *under, err = sessionPrefixes(tx, sessions, under == &all); err != nil {
				return 0, err
			}
		}
		tail := key(r.zone, append([]byte{receiptPrefix}, request...))
		was, _ := reservedBy(request, ledger, tail, *under, before, false)
		is, err := reservedBy(request, ledger, tail, *under, after, true)
		if err != nil {
			return 0, err
		}
		d += is - was
	}
	return d, nil
}

// sessionPrefixes are the staging prefixes of the sessions the update stages
// in and, with indexed, of every session that has staged rows, in session
// order: the scan meets a request's copies in that order. Never nil.
func sessionPrefixes(tx *Tx, touched []string, indexed bool) ([]string, error) {
	sessions := slices.Clone(touched)
	if indexed {
		held, err := stagingSessions(tx)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, held...)
	}
	slices.Sort(sessions)
	sessions = slices.Compact(sessions)
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = key(stagingBucket, []byte(s))
	}
	return out, nil
}

// affectedRequest is a request whose reservation the update can change: the
// staging element of its zone, and whether the update stages a copy of it.
type affectedRequest struct {
	zone    []byte
	staged  bool
	changed int // rows of the update that held a staged copy of it
}

// affectedRequests are the requests whose reservation the update can change
// and the sessions the update stages in.
func affectedRequests(tx *Tx, paths []string) (map[string]affectedRequest, []string, error) {
	staging, operations := key(stagingBucket), key(operationsBucket)
	affected := map[string]affectedRequest{}
	var sessions []string
	for _, p := range paths {
		if p != staging && strings.HasPrefix(p, staging) {
			elems := split(p[len(staging):])
			sessions = append(sessions, string(elems[0]))
			if len(elems) == 3 && len(elems[2]) > 0 && elems[2][0] == receiptPrefix {
				request := string(elems[2][1:])
				r := affected[request]
				r.zone, r.staged = elems[1], r.staged || tx.pending[p] != nil
				if _, existed := tx.committedSize(p); existed {
					r.changed++
				}
				affected[request] = r
			}
			continue
		}
		if p == operations || !strings.HasPrefix(p, operations) {
			continue
		}
		elems := split(p[len(operations):])
		if len(elems) != 1 {
			continue
		}
		request := string(elems[0])
		if _, ok := affected[request]; ok {
			continue
		}
		v, ok := pendingValue(tx, p)
		if !ok {
			v, ok = committedValue(tx, p)
		}
		zone, err := receiptZone(v, ok)
		if err != nil {
			return nil, nil, err
		}
		affected[request] = affectedRequest{zone: zone}
	}
	return affected, sessions, nil
}

// reservedBy is what one request reserves as get sees the rows: nothing once
// it is in the ledger, else its key and its first staged copy. With check,
// copies that differ from the ledger or from each other are refused.
func reservedBy(request, ledger, tail string, sessions []string, get func(string) ([]byte, bool), check bool) (int64, error) {
	prior, held := get(ledger)
	if held && !check {
		return 0, nil
	}
	var first []byte
	for _, session := range sessions {
		v, ok := get(session + tail)
		if !ok {
			continue
		}
		if check && (held && !bytes.Equal(prior, v) || first != nil && !bytes.Equal(first, v)) {
			return 0, ErrConflict
		}
		if first == nil {
			first = v
		}
	}
	if held || first == nil {
		return 0, nil
	}
	return int64(len(request) + len(first)), nil
}

// receiptZone is the staging element of the zone a ledger receipt names.
func receiptZone(raw []byte, ok bool) ([]byte, error) {
	if !ok {
		return nil, fmt.Errorf("ledger receipt changed without a value: %w", ErrConflict)
	}
	var r RequestReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	zone, err := tree.NewZoneKey(r.Collection, r.Location)
	if err != nil {
		return nil, err
	}
	return zoneBytes(zone), nil
}

// accountUsage keeps the stored usage exact for an update that changed
// counted rows without writing the usage itself (a freeze writing transfer
// snapshots, a region install dropping staging): the incremental count
// starts from it. Updates that write the usage (writes, capacity checks,
// ingests) are left alone. It enforces nothing, as before.
func (s *Store) accountUsage(tx *Tx) error {
	if _, written := tx.pending[meta(usageKey)]; written {
		return nil
	}
	counted := false
	for p := range tx.pending {
		if countedPath(p) {
			counted = true
			break
		}
	}
	if !counted && tx.delta == 0 {
		return nil
	}
	used, ok, err := s.usageAfter(tx)
	if err != nil || !ok {
		return err
	}
	if verifyUsage {
		scanned, err := s.dataUsage(tx)
		if err != nil {
			return err
		}
		if scanned != used {
			return fmt.Errorf("incremental usage %+v, scanned %+v", used, scanned)
		}
	}
	return tx.putJSON(meta(usageKey), used)
}

var countedPrefixes = []string{key(liveBucket), key(historyBucket), key(operationsBucket), key(stagingBucket), key(transferBucket)}

func countedPath(p string) bool {
	for _, prefix := range countedPrefixes {
		if p != prefix && strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// verifyUsage makes every capacity check also scan and compare: on in tests
// (a benchmark that measures handoffs sets INDEXUS_BENCH_NO_USAGE_SCAN),
// so that every test is a differential test of the incremental count.
var verifyUsage = testing.Testing() && os.Getenv("INDEXUS_BENCH_NO_USAGE_SCAN") == ""

// usageScans counts full scans of the usage, for tests.
var usageScans atomic.Int64

// capacityChecks counts the capacity checks, for tests: one per transaction.
var capacityChecks atomic.Int64

// checkCapacity bounds encoded keys and values of every data allocation,
// including retry receipts, mutation logs and staging. B-tree pages and control
// metadata need additional filesystem headroom; this is not a filesystem quota.
func (s *Store) checkCapacity(tx *Tx) error {
	capacityChecks.Add(1)
	used, ok, err := s.usageAfter(tx)
	if err != nil {
		return err
	}
	if !ok || verifyUsage {
		scanned, err := s.dataUsage(tx)
		if err != nil {
			return err
		}
		// Every test is a differential test of the incremental count.
		if ok && scanned != used {
			return fmt.Errorf("incremental usage %+v, scanned %+v", used, scanned)
		}
		used = scanned
	}
	return s.persistCapacity(tx, used)
}
