package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/indexus/go-indexus-core/internal/tree"
	"slices"
	"sync/atomic"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// rowBytesKey counts what the rows in zonelog take against capacity, in the
// same units as bbolt's live bucket: key 1+len(id), value the JSON record.
var rowBytesKey = []byte("zonelog-row-bytes")

// Record rows are zonelog records of their collection and location. An
// update buffers its puts and clears in its Tx (records) and commits them
// with its control rows, in one batch (commit).
//
// A tombstone, and a record with more metrics than zonelog keeps in
// columns, is an opaque row whose payload is the record's JSON. Every read
// loads it. A tombstone is also uncounted: zonelog's zone sums count live
// records only. A wide record counts, without its metrics.
type pendingZone struct {
	zone tree.ZoneKey
	rows map[string]*Record // nil: removed in this update
	// logs keeps the zonelog form putRecord made of a row to size it: the
	// commit writes that one and does not encode the payload again.
	logs map[string]zonelog.Record
}

var errSkipZone = errors.New("skip zone")

// row is one stored record. size is what it takes against capacity,
// whether or not the payload was read.
type row struct {
	id     string
	record *Record
	size   int
}

// logEncodes counts the conversions of a record to its zonelog form, for tests.
var logEncodes atomic.Int64

func toLog(r Record) (zonelog.Record, error) {
	logEncodes.Add(1)
	out := zonelog.Record{Collection: r.Collection, Location: r.Location, ID: r.ID}
	var err error
	if r.Deleted {
		// The record's JSON is its payload: a tombstone, kept for snapshots
		// and seals and out of the sums.
		out.Opaque, out.Uncounted, out.Weight = true, r.Deleted, r.Weight
		out.Payload, err = json.Marshal(r)
		return out, err
	}
	out.Metrics, out.Weight = r.Metrics, r.Weight
	if r.Binary {
		out.Binary = true
		err = json.Unmarshal(r.Payload, &out.Payload)
		return out, err
	}
	if len(r.Payload) > 0 {
		// The bytes json.Marshal stores in bbolt: compact, HTML escaped.
		out.Payload, err = json.Marshal(r.Payload)
	}
	return out, err
}

func fromLog(z zonelog.Record) (Record, error) {
	logDecodes.Add(1)
	if z.Opaque {
		var r Record
		err := json.Unmarshal(z.Payload, &r)
		return r, err
	}
	// A memtable row hands out its own slices: copy them.
	r := Record{Collection: z.Collection, Location: z.Location, ID: z.ID, Metrics: slices.Clone(z.Metrics)}
	if z.Weight != 1 {
		r.Weight = z.Weight
	}
	if z.Binary {
		raw, err := json.Marshal(z.Payload)
		r.Binary, r.Payload = true, raw
		return r, err
	}
	if len(z.Payload) > 0 {
		r.Payload = json.RawMessage(bytes.Clone(z.Payload))
	}
	return r, nil
}

// MaxMetrics is the most metrics a record has: a write or a batch with more
// is refused, never stored without them. It is zonelog's (4,096, an
// embedding of a few thousand dimensions); a zonelog page ends at 2 MB of
// metric columns.
const MaxMetrics = zonelog.MaxMetrics

// logSize is what a row takes in zonelog, as capacity counts it: a header,
// its metrics and its payload (the record's JSON when weight is -1). It is
// not the JSON length bbolt counts: on this engine the JSON is not stored.
func logSize(metrics, payload int) int { return 16 + 8*metrics + payload }

func recordSize(r Record) (int, error) {
	rec, err := toLog(r)
	return logRecordSize(rec), err
}

func logRecordSize(rec zonelog.Record) int { return logSize(len(rec.Metrics), len(rec.Payload)) }

// logRow makes a row of a zonelog record whose payload has payloadBytes
// bytes, read or not.
func logRow(z zonelog.Record, payloadBytes int) (*row, error) {
	r, err := fromLog(z)
	if err != nil {
		return nil, err
	}
	return &row{id: r.ID, record: &r, size: logSize(len(z.Metrics), payloadBytes)}, nil
}

func (tx *Tx) zoneRows(zone tree.ZoneKey, after string, payloads bool, visit func(*row) (bool, error)) error {
	var ids []string
	p := tx.records[string(zoneBytes(zone))]
	if p != nil {
		for id := range p.rows {
			if id > after {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
	}
	stop := false
	// emit visits the pending rows before id ("": all that are left).
	emit := func(before string) error {
		for len(ids) > 0 && !stop && (before == "" || ids[0] < before) {
			id := ids[0]
			ids = ids[1:]
			if r := p.rows[id]; r != nil {
				size, err := recordSize(*r)
				if err != nil {
					return err
				}
				more, err := visit(&row{id: id, record: r, size: size})
				if stop = !more; err != nil {
					return err
				}
			}
		}
		return nil
	}
	var failed error
	var from *zonelog.Record
	if after != "" {
		from = &zonelog.Record{Location: zone.Location, ID: after}
	}
	err := scanPaged(tx.reader, zone.Collection, zone.Location, from, &zonelog.IDRange{}, loader(payloads), func(rec zonelog.Record, size int) bool {
		if failed = emit(rec.ID); failed != nil || stop {
			return false
		}
		if p != nil {
			if _, ok := p.rows[rec.ID]; ok {
				return true // emitted, or removed, from pending
			}
		}
		r, err := logRow(rec, size)
		if err != nil {
			failed = err
			return false
		}
		more, err := visit(r)
		failed, stop = err, !more
		return more && err == nil
	})
	if err = errors.Join(err, failed); err != nil || stop {
		return err
	}
	return emit("")
}

// loader reads a payload when asked to, and always for a row whose payload
// is the record.
func loader(payloads bool) func(zonelog.Record) bool {
	return func(r zonelog.Record) bool { return payloads || r.Opaque }
}

var errWalkPending = errors.New("storage: a walk over zones in an update that wrote rows")

func (tx *Tx) walk(query *tree.ZoneKey, from *tree.ZoneKey, after string, payloads bool, visit func(tree.ZoneKey, *row) (bool, error)) error {
	if len(tx.records) > 0 {
		return errWalkPending
	}
	for _, collection := range tx.reader.Collections() {
		if query != nil && collection != query.Collection || from != nil && collection < from.Collection {
			continue
		}
		root := "@"
		if query != nil {
			root = query.Location
		}
		var current tree.ZoneKey
		started, skip, stop := false, false, false
		var start *zonelog.Record
		// A walk from a zone visits that zone first, even with no row left
		// behind after: a paged read checks where it resumes.
		if from != nil && collection == from.Collection {
			start = &zonelog.Record{Location: from.Location, ID: after}
			current, started = *from, true
			more, err := visit(current, nil)
			if skip = err == errSkipZone; !skip && (!more || err != nil) {
				return err
			}
		}
		var failed error
		next := func(rec zonelog.Record, size int) bool {
			if !started || rec.Location != current.Location {
				zone, err := tree.NewZoneKey(collection, rec.Location)
				if err != nil {
					failed = err
					return false
				}
				current, started = zone, true
				more, err := visit(current, nil)
				if skip = err == errSkipZone; !skip && (!more || err != nil) {
					failed, stop = err, true
					return false
				}
			}
			if skip {
				return true
			}
			r, err := logRow(rec, size)
			if err == nil {
				var more bool
				more, err = visit(current, r)
				if err == errSkipZone {
					skip = true
					return true
				}
				stop = !more
			}
			failed = err
			return !stop && err == nil
		}
		err := scanPaged(tx.reader, collection, root, start, nil, loader(payloads), next)
		if err = errors.Join(err, failed); err != nil || stop {
			return err
		}
	}
	return nil
}

func (tx *Tx) record(zone tree.ZoneKey, id string) (*row, error) {
	if p := tx.records[string(zoneBytes(zone))]; p != nil {
		if r, ok := p.rows[id]; ok {
			if r == nil {
				return nil, nil
			}
			size, err := recordSize(*r)
			return &row{id: id, record: r, size: size}, err
		}
	}
	rec, ok, err := tx.reader.Get(zone.Collection, zone.Location, id)
	if err != nil || !ok {
		return nil, err
	}
	return logRow(rec, len(rec.Payload))
}

func (tx *Tx) setRecord(zone tree.ZoneKey, id string, r *Record) {
	if tx.records == nil {
		tx.records = map[string]*pendingZone{}
	}
	p := tx.records[string(zoneBytes(zone))]
	if p == nil {
		p = &pendingZone{zone: zone, rows: map[string]*Record{}, logs: map[string]zonelog.Record{}}
		tx.records[string(zoneBytes(zone))] = p
	}
	p.rows[id] = r
	delete(p.logs, id)
}

// setLogged is setRecord for a row whose zonelog form is already made.
func (tx *Tx) setLogged(zone tree.ZoneKey, r *Record, rec zonelog.Record) {
	tx.setRecord(zone, r.ID, r)
	tx.records[string(zoneBytes(zone))].logs[r.ID] = rec
}

// putRecord buffers r, and returns the row it replaces (nil: none) and what
// that changes in usage.
func (tx *Tx) putRecord(zone tree.ZoneKey, r Record) (*row, int64, error) {
	old, err := tx.record(zone, r.ID)
	if err != nil {
		return nil, 0, err
	}
	rec, err := toLog(r)
	if err != nil {
		return nil, 0, err
	}
	before := -1
	if old != nil {
		before = old.size
	}
	delta := rowDelta(len(r.ID), before, logRecordSize(rec))
	tx.delta += delta
	tx.setLogged(zone, &r, rec)
	return old, delta, nil
}

// putCleared buffers r in a zone this update emptied with clearZone: every
// committed row of the zone is already removed in pending, so the row r
// replaces, if any, is a pending one. A staged zone installed over an empty
// one read the store once per record, under the writer, to find nothing.
func (tx *Tx) putCleared(zone tree.ZoneKey, r Record) error {
	if p := tx.records[string(zoneBytes(zone))]; p != nil && p.rows[r.ID] != nil {
		_, _, err := tx.putRecord(zone, r)
		return err
	}
	rec, err := toLog(r)
	if err != nil {
		return err
	}
	tx.delta += rowDelta(len(r.ID), -1, logRecordSize(rec))
	tx.setLogged(zone, &r, rec)
	return nil
}

func (tx *Tx) clearZone(zone tree.ZoneKey) error {
	// Deleting a zone needs IDs and encoded sizes only, including opaque
	// tombstones. Decoding those old payloads under writeMu serves no purpose.
	var gone []*row
	pending := tx.records[string(zoneBytes(zone))]
	if pending != nil {
		for id, r := range pending.rows {
			if r != nil {
				size, err := recordSize(*r)
				if err != nil {
					return err
				}
				gone = append(gone, &row{id: id, size: size})
			}
		}
	}
	err := scanPaged(tx.reader, zone.Collection, zone.Location, nil, &zonelog.IDRange{},
		func(zonelog.Record) bool { return false },
		func(r zonelog.Record, size int) bool {
			if pending != nil {
				if _, replaced := pending.rows[r.ID]; replaced {
					return true
				}
			}
			gone = append(gone, &row{id: r.ID, size: logSize(len(r.Metrics), size)})
			return true
		})
	if err != nil {
		return err
	}
	for _, r := range gone {
		tx.delta -= int64(1 + len(r.id) + r.size)
		tx.setRecord(zone, r.id, nil)
	}
	return nil
}

func (tx *Tx) dropZone(zone tree.ZoneKey) error {
	if err := tx.clearZone(zone); err != nil {
		return err
	}
	return tx.drop(string(liveZone(zone)))
}

func (tx *Tx) rowBytes() int64 { return storedRowBytes(tx) + tx.delta }

func storedRowBytes(tx *Tx) int64 {
	return int64(tx.number8(meta(rowBytesKey)))
}

// commit ends an update: with err nil it commits the record rows and the
// control rows it wrote as one zonelog batch; otherwise it forgets them.
func (tx *Tx) commit(err error) error {
	if tx.reader.err != nil {
		return tx.reader.err
	}
	pending, delta := tx.records, tx.delta
	if err != nil {
		return err
	}
	if err := tx.indexInventory(); err != nil {
		return err
	}
	// Every staged row is written or deleted in an update that ends here:
	// the index of the sessions follows them in the same batch.
	if err := tx.indexStaging(); err != nil {
		return err
	}
	if verifyUsage && tx.live() {
		// Every test is a differential test of the staging index.
		if err := checkStagingIndex(tx); err != nil {
			return err
		}
		if err := checkCopies(tx); err != nil {
			return err
		}
	}
	if delta != 0 {
		if err := tx.put(meta(rowBytesKey), number(uint64(storedRowBytes(tx)+delta))); err != nil {
			return err
		}
	}
	var batch []zonelog.Record
	for _, key := range sortedKeys(pending) {
		p := pending[key]
		for _, id := range sortedKeys(p.rows) {
			rec := zonelog.Record{Collection: p.zone.Collection, Location: p.zone.Location, ID: id, Deleted: true}
			if logged, ok := p.logs[id]; ok {
				rec = logged
			} else if r := p.rows[id]; r != nil {
				if rec, err = toLog(*r); err != nil {
					return err
				}
			}
			batch = append(batch, rec)
		}
	}
	if batch = append(batch, tx.controlRows()...); len(batch) == 0 {
		return nil
	}
	// Usage accounting above can read too; do not commit after a late failure.
	if tx.reader.err != nil {
		return tx.reader.err
	}
	countCommit(batch)
	if err := tx.db.Apply(batch); err != nil {
		return err
	}
	if tx.owner != nil {
		tx.owner.noteStaged(tx)
		tx.owner.noteCopies(tx)
	}
	return nil
}

// scanPage is how many records scanPaged reads under zonelog's lock.
const scanPage = 512

// scanFirstPage is the first page of a paged scan.
const scanFirstPage = 8

// scanPaged is ScanWith with visit called outside zonelog's read lock, a
// page of records at a time: a visit may read the store again (control
// rows), and a read lock taken twice deadlocks once a writer waits between
// the two. Read transactions pass a zonelog.Snapshot, so every page sees the
// same committed version. The physical ID range bounds every page, and a
// small first page keeps short scans cheap.
func scanPaged(db zonelog.Reader, collection, location string, after *zonelog.Record, ids *zonelog.IDRange, load func(zonelog.Record) bool, visit func(zonelog.Record, int) bool) error {
	type item struct {
		r    zonelog.Record
		size int
	}
	var page []item
	// The first page is small and the next ones grow: a visit that stops at
	// its first row (a seek) must not read scanPage rows to get it.
	for limit := scanFirstPage; ; limit = min(4*limit, scanPage) {
		page = page[:0]
		err := db.ScanWith(collection, location, after, ids, load, func(r zonelog.Record, size int) bool {
			page = append(page, item{r, size})
			return len(page) < limit
		})
		if err != nil {
			return err
		}
		for _, it := range page {
			if !visit(it.r, it.size) {
				return nil
			}
		}
		if len(page) < limit {
			return nil
		}
		last := page[len(page)-1].r
		after = &zonelog.Record{Location: last.Location, ID: last.ID}
	}
}

// rowsOf reads one zone's rows into a slice.
func rowsOf(tx *Tx, z tree.ZoneKey, payloads bool) ([]*row, error) {
	var out []*row
	err := tx.zoneRows(z, "", payloads, func(r *row) (bool, error) {
		out = append(out, r)
		return true, nil
	})
	return out, err
}

// hasZones reports whether any zone holds rows.
func hasZones(tx *Tx) (bool, error) {
	found := false
	err := tx.walk(nil, nil, "", false, func(tree.ZoneKey, *row) (bool, error) {
		found = true
		return false, nil
	})
	return found, err
}
