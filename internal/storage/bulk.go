package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/prep"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

var (
	// ErrBulkOverlap: the zone already holds records and the batch does not
	// replace them.
	ErrBulkOverlap = fmt.Errorf("%w: the zone holds records; ingest with replace", ErrConflict)
	// ErrBulkInvalid: the batch is not what its manifest says, or not a batch.
	ErrBulkInvalid = errors.New("invalid batch")
)

// bulkBucket keeps, under meta, the receipt of every installed batch.
var bulkBucket = []byte("bulk-v1")

// BulkReceipt is what a node keeps of a batch: one per batch, not per record.
type BulkReceipt struct {
	Batch      string // content key: the collection and the hashes of the parts
	Source     string // the manifest's id: what the batch was made from
	Collection string
	Zone       string // every record is at or below it
	Replace    bool
	Records    int64
	Replaced   int64 // old records the batch replaced or deleted
	Bytes      int64 // what its rows take against capacity
	At         time.Time
}

type BulkResult struct {
	BulkReceipt
	Zones   int64
	Already bool // the same batch was installed before: nothing changed
}

// BatchKey names a batch by its content: the same parts for the same
// collection are the same batch, whatever produced them.
func BatchKey(m prep.Manifest) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%d", m.Format, m.Collection, m.Metrics)
	if m.PayloadEncoding == "binary" {
		// The same bytes as JSON are another batch; JSON keys stay as they were.
		fmt.Fprint(h, "\x00binary")
	}
	for _, p := range m.Parts {
		fmt.Fprintf(h, "\x00%s", p.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// batchZone is the deepest zone that holds every record of a batch: the
// common prefix of its first and last location.
func batchZone(m prep.Manifest) (string, error) {
	if len(m.Parts) == 0 {
		return "", fmt.Errorf("%w: a batch without parts", ErrBulkInvalid)
	}
	first, _, _ := strings.Cut(m.Parts[0].First, "/")
	last, _, _ := strings.Cut(m.Parts[len(m.Parts)-1].Last, "/")
	n := 0
	for n < len(first) && n < len(last) && first[n] == last[n] && first != "@" {
		n++
	}
	if n == 0 {
		return "@", nil
	}
	return first[:n], nil
}

// IngestBulk installs a prepared batch (docs/ingest/pipeline.md, IXP1 parts)
// from the directory dir, for one collection, in one cut. It reads each file
// once, front to back (as IngestBulkStream reads a stream), and checks the
// whole batch before the cut: part hashes, block CRCs, key order, record count,
// the zone sums of zones.ixp at its shallowest depth, and the writable
// authority of this disk for every zone a record is in, under the write lock,
// so no write, fence or handoff commits meanwhile; readers go on. Without
// replace the batch's zone must hold no record here; with replace the batch
// is the zone's new content. The rows go into one zonelog segment; nothing
// is visible before the manifest rename, and the receipt is a row of the same
// cut. The batch gets no history, no per-record receipt and no sequence: a
// later write to one of its records starts the history of that zone. The
// same batch again is a no-op.
func (s *Store) IngestBulk(ctx context.Context, dir string, replace bool) (BulkResult, error) {
	m, err := prep.ReadManifest(dir)
	if err != nil {
		return BulkResult{}, fmt.Errorf("%w: %v", ErrBulkInvalid, err)
	}
	files := &dirFiles{dir: dir}
	defer files.close()
	return s.ingest(ctx, m, files.next, replace)
}

// ingest installs the batch of manifest m whose files next hands over, in
// manifest order: the parts, then zones.ixp.
func (s *Store) ingest(ctx context.Context, m prep.Manifest, next func(name string) (io.Reader, error), replace bool) (BulkResult, error) {
	zone, err := batchZone(m)
	if err == nil {
		_, err = tree.NewZoneKey(m.Collection, zone)
	}
	if err != nil || m.Metrics > MaxMetrics || m.Records > math.MaxInt64 {
		return BulkResult{}, fmt.Errorf("%w: %s %s: %v (at most %d metrics)", ErrBulkInvalid, m.Collection, zone, err, MaxMetrics)
	}
	// DESC-1: a batch of a described collection carries one metric per
	// column. Its rows are read record by record by the typed fold, so the
	// batch's zone sums are not used for it.
	if c, _ := s.CollectionOf(m.Collection); c != nil && m.Metrics != len(c.Desc.Schema) {
		return BulkResult{}, fmt.Errorf("%w: %w: %s has %d columns, the batch %d metrics", ErrBulkInvalid, ErrDescription, m.Collection, len(c.Desc.Schema), m.Metrics)
	}
	result := BulkResult{BulkReceipt: BulkReceipt{Batch: BatchKey(m), Source: m.ID, Collection: m.Collection, Zone: zone, Replace: replace, Records: int64(m.Records)}}
	s.locks.write.wait()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.locks.write.took()
	defer s.locks.write.released()
	var control *causalSnapshot
	var usage protocol.ResourceCounters
	err = s.view(func(tx *Tx) error {
		if raw := bulkReceipt(tx, result.Batch); raw != nil {
			result.Already = true
			return json.Unmarshal(raw, &result.BulkReceipt)
		}
		state, err := readCausal(tx)
		if err != nil {
			return err
		}
		// A handoff or a fold in progress copies zones by their sequence and
		// history: a batch has neither, so it waits for them to finish.
		if state.Moving != nil || pendingCausal(state, tx) || coldBusy(tx) {
			return fmt.Errorf("%w: a handoff is in progress", ErrNotReady)
		}
		if control, err = s.causalSnapshot(tx); err != nil {
			return err
		}
		usage, err = s.storedDataUsage(tx)
		return err
	})
	if err != nil || result.Already {
		return result, err
	}
	var old int64
	if replace {
		if old, err = s.zoneRowBytes(m.Collection, zone); err != nil {
			return result, err
		}
	}
	b, err := s.db.BeginBulk(m.Collection, zone, replace, result.Records)
	if errors.Is(err, zonelog.ErrNotEmpty) {
		err = ErrBulkOverlap
	}
	if err != nil {
		return result, err
	}
	defer b.Abort()
	note, _ := json.Marshal(result.BulkReceipt)
	extra := []zonelog.Record{{Collection: zonelog.Control, Location: "@", ID: bulkReceiptID(result.Batch), Payload: note}}
	// Cursor invalidation belongs to the same atomic cut as the new data.
	versions := s.transaction(s.db, true)
	if err := versions.advanceInventory(map[string]bool{m.Collection: true}); err != nil {
		return result, err
	}
	if err := b.Also(append(extra, versions.controlRows()...)...); err != nil {
		return result, err
	}
	if err := s.writeBulkRows(ctx, m, next, b, control, &result); err != nil {
		return result, err
	}
	used := usage
	used.Owned += result.Bytes - old
	if used.PhysicalLoad() > s.capacity {
		return result, ErrDataCapacity
	}
	result.Replaced, result.At = b.Replaced(), time.Now().UTC()
	// The cut, then the receipt and the counters, serialized under writeMu.
	// A crash between the two leaves the rows and the cut's own receipt row;
	// Open counts them (reconcileBulk).
	s.spatial.mu.Lock()
	commitErr := b.Commit()
	s.invalidateSpatialLocked() // replacement may remove arbitrary canonical zones
	s.spatial.mu.Unlock()
	if commitErr != nil {
		return result, commitErr
	}
	tx := s.transaction(s.db, true)
	return result, tx.commit(s.noteBulk(tx, result.BulkReceipt, result.Bytes-old))
}

func bulkReceiptID(batch string) string { return "bulk/" + batch }

// bulkReceipt is the stored receipt of a batch, or nil.
func bulkReceipt(tx *Tx, batch string) []byte {
	return tx.value(key(metaBucket, bulkBucket, []byte(batch)))
}

// noteBulk records an installed batch and what its rows changed in usage.
func (s *Store) noteBulk(tx *Tx, receipt BulkReceipt, delta int64) error {
	if err := tx.putJSON(key(metaBucket, bulkBucket, []byte(receipt.Batch)), receipt); err != nil {
		return err
	}
	usage, err := s.storedDataUsage(tx)
	if err != nil {
		return err
	}
	usage.Owned += delta
	if err := tx.putJSON(meta(usageKey), usage); err != nil {
		return err
	}
	return tx.put(meta(rowBytesKey), number(uint64(storedRowBytes(tx)+delta)))
}

// writeBulkRows streams the batch into the segment, checking each record.
func (s *Store) writeBulkRows(ctx context.Context, m prep.Manifest, next func(string) (io.Reader, error), b *zonelog.Bulk, control *causalSnapshot, result *BulkResult) error {
	// The sums of zones.ixp at its shallowest depth are checked against the
	// records, as a reader that computed them independently would. zones.ixp
	// comes last: the records are summed on the way.
	sums := map[prep.Head]*prep.Row{}
	depth := -1
	if m.Zones != nil {
		depth = m.Zones.Depths[0]
	}
	binary := m.PayloadEncoding == "binary"
	if !binary && m.PayloadEncoding != "" && m.PayloadEncoding != "json" {
		return fmt.Errorf("%w: payload encoding %q", ErrBulkInvalid, m.PayloadEncoding)
	}
	var head prep.Head
	location, n := "", 0
	visit := func(row prep.Row) error {
		if n++; n%65536 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if n == 1 || row.Head != head {
			head, location = row.Head, row.Head.Location()
			z, err := tree.NewZoneKey(m.Collection, location)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrBulkInvalid, err)
			}
			// No transfer is in progress (checked above): no per-zone rows.
			if err := s.writableIn(nil, control, z); err != nil {
				return fmt.Errorf("zone %s/%s: %w", m.Collection, location, err)
			}
			result.Zones++
		}
		if row.Weight < 0 || math.IsNaN(row.Weight) || math.IsInf(row.Weight, 0) {
			return fmt.Errorf("%w: record %s/%s: weight %v", ErrBulkInvalid, location, row.ID, row.Weight)
		}
		for _, v := range row.Metrics {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("%w: record %s/%s: metric %v is not a number", ErrBulkInvalid, location, row.ID, v)
			}
		}
		payload, err := row.Payload, error(nil)
		if !binary {
			payload, err = canonicalPayload(row.Payload)
		}
		if err != nil {
			return fmt.Errorf("%w: record %s/%s: payload: %v", ErrBulkInvalid, location, row.ID, err)
		}
		if depth >= 0 && depth <= head.Depth() {
			z := sums[head.Prefix(depth)]
			if z == nil {
				z = &prep.Row{Metrics: make([]float64, 1+m.Metrics)}
				sums[head.Prefix(depth)] = z
			}
			z.Weight += row.Weight // as the producer summed them: 0 stays 0 here
			z.Metrics[0]++
			for j, v := range row.Metrics {
				z.Metrics[1+j] += v
			}
		}
		result.Bytes += int64(1 + len(row.ID) + logSize(len(row.Metrics), len(payload)))
		return b.AddHead(head[:], row.ID, WeightOf(row.Weight), row.Metrics, payload, binary && len(payload) > 0)
	}
	invalid := func(err error) error {
		if err == nil || errors.Is(err, ErrBulkInvalid) || errors.Is(err, ErrNotOwner) || errors.Is(err, ErrFrozen) || errors.Is(err, ErrNotReady) || ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w: %v", ErrBulkInvalid, err)
	}
	var records uint64
	for _, part := range m.Parts {
		r, err := next(part.File)
		if err == nil {
			err = prep.ReadPart(r, part, m.Metrics, visit)
		}
		if err != nil {
			return invalid(err)
		}
		records += part.Records
	}
	if records != m.Records || uint64(n) != m.Records {
		return fmt.Errorf("%w: %d records, the manifest says %d", ErrBulkInvalid, n, m.Records)
	}
	if m.Zones == nil {
		return nil
	}
	check, err := shallowZones(next, m)
	if err != nil {
		return invalid(err)
	}
	if len(sums) != len(check) {
		return fmt.Errorf("%w: %d zones of depth %d hold records, zones.ixp has %d", ErrBulkInvalid, len(sums), depth, len(check))
	}
	for at, want := range check {
		got := sums[at]
		if got == nil || !near(got.Weight, want.Weight) || !slices.EqualFunc(got.Metrics, want.Metrics, near) {
			return fmt.Errorf("%w: zone %s of zones.ixp does not sum its records", ErrBulkInvalid, at.Location())
		}
	}
	return nil
}

// near allows float sums added in another order to differ in the last bits.
func near(a, b float64) bool {
	return a == b || math.Abs(a-b) <= 1e-9*max(math.Abs(a), math.Abs(b))
}

// shallowZones reads zones.ixp and keeps its rows at its shallowest depth.
func shallowZones(next func(string) (io.Reader, error), m prep.Manifest) (map[prep.Head]*prep.Row, error) {
	r, err := next(m.Zones.File)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(r, 1<<30))
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != m.Zones.SHA256 {
		return nil, errors.New("zones.ixp: sha256 differs from the manifest")
	}
	pr, err := prep.Open(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	if pr.M != m.Metrics+1 {
		return nil, fmt.Errorf("zones.ixp has %d columns", pr.M)
	}
	out := map[prep.Head]*prep.Row{}
	err = pr.Each(func(r prep.Row) error {
		if r.Head.Depth() == m.Zones.Depths[0] {
			out[r.Head] = &prep.Row{Head: r.Head, Weight: r.Weight, Metrics: slices.Clone(r.Metrics)}
		}
		return nil
	})
	return out, err
}

// dirFiles hands the files of a batch directory one after the other.
type dirFiles struct {
	dir  string
	open *os.File
}

func (d *dirFiles) next(name string) (io.Reader, error) {
	d.close()
	if name != filepath.Base(name) {
		return nil, fmt.Errorf("the manifest names %q", name)
	}
	f, err := os.Open(filepath.Join(d.dir, name))
	d.open = f
	return f, err
}

func (d *dirFiles) close() {
	if d.open != nil {
		d.open.Close()
		d.open = nil
	}
}

// zoneRowBytes is what the rows of a zone take against capacity.
func (s *Store) zoneRowBytes(collection, zone string) (int64, error) {
	var total int64
	var failed error
	err := s.db.ScanWith(collection, zone, nil, nil, loader(false), func(rec zonelog.Record, size int) bool {
		r, err := logRow(rec, size)
		if failed = err; err != nil {
			return false
		}
		total += int64(1 + len(r.id) + r.size)
		return true
	})
	return total, errors.Join(err, failed)
}

// reconcileBulk finishes, at open, a batch whose cut committed and whose
// control rows did not (a crash between the two): it records the receipt
// and recounts the rows.
func (s *Store) reconcileBulk() error {
	var missing []BulkReceipt
	err := s.view(func(tx *Tx) error {
		// The cut's receipts first, then the control rows: a read inside
		// a zonelog scan would take its lock twice.
		var cut []zonelog.Record
		err := s.db.Scan(zonelog.Control, "@", nil, func(rec zonelog.Record) bool {
			if strings.HasPrefix(rec.ID, "bulk/") {
				cut = append(cut, rec)
			}
			return true
		})
		for _, rec := range cut {
			if err == nil && bulkReceipt(tx, strings.TrimPrefix(rec.ID, "bulk/")) == nil {
				var receipt BulkReceipt
				err = json.Unmarshal(rec.Payload, &receipt)
				missing = append(missing, receipt)
			}
		}
		return err
	})
	if err != nil || len(missing) == 0 {
		return err
	}
	var total int64
	for _, collection := range s.db.Collections() {
		n, err := s.zoneRowBytes(collection, "@")
		if err != nil {
			return err
		}
		total += n
	}
	return s.update(func(tx *Tx) error {
		delta := total - storedRowBytes(tx)
		for i, receipt := range missing {
			if i > 0 {
				delta = 0
			}
			if err := s.noteBulk(tx, receipt, delta); err != nil {
				return err
			}
		}
		return nil
	})
}

// canonicalPayload checks that a payload is JSON and returns it in the form
// a write stores: compact, HTML escaped. Most payloads already are.
func canonicalPayload(p []byte) ([]byte, error) {
	if len(p) == 0 {
		return nil, nil
	}
	plain := true
	for _, c := range p {
		if c <= ' ' || c == '<' || c == '>' || c == '&' || c == 0xE2 {
			plain = false
			break
		}
	}
	if plain && json.Valid(p) {
		return p, nil
	}
	return json.Marshal(json.RawMessage(p))
}
