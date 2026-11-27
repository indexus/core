package storage

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"slices"
	"strings"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// The control state of a store (meta, authority, fences, zone sequences,
// receipts, history, staging, counters) is rows of the zonelog collection
// kvCollection. A transaction reads the committed rows under its own
// writes; an update commits its writes and the record rows of the same
// update as one zonelog batch: one frame, one fsync, all or nothing.
//
// A row's id is its path, the names of the buckets bbolt kept it in and its
// key (key()), each escaped (0x00 as 00 01) and ended by 00 00: ids sort as
// bbolt sorted them, and the rows below a path follow it. The payload is
// 'v' and the value (an empty value is a reservation: staging/<operation>).
const kvCollection = "\x00kv"

var errKVReadOnly = errors.New("storage: write in a read transaction")

// Tx is one read or update of the control state.
type Tx struct {
	owner     *Store // per-disk decoding caches; absent on raw/offline transactions
	db        *zonelog.Store
	reader    *txReader
	ownReader txReader // what reader points to: one allocation less per transaction
	writable  bool
	version   uint64
	pending   map[string][]byte // path → payload; nil: deleted
	sizes     map[string]int    // committed control payload lengths learned from key scans and point reads
	absent    map[string]bool   // control paths a point read found no committed row at
	below     map[string]int    // path → rows the update put below it
	// sorted and recent index the pending paths for rows: sorted holds most
	// of them in order, recent the last ones added until it is merged in.
	// A lookup by prefix is then a binary search, not a pass over every
	// pending path: per-zone lookups in one large update (an install, a
	// staged region) were quadratic in its size.
	sorted, recent []string
	records        map[string]*pendingZone
	// certified is, in a mount, the staged copies certified outside the
	// writer: prepareMount collects them, mountPreparedTx hands them to the
	// install once it has checked that staging did not change in between.
	certified map[tree.ZoneKey]certifiedCopy
	// staged is, for each staging session this update touched, whether a
	// staged row is left of it (indexStaging).
	staged map[string]bool
	// copied is what this update changes in the staged copies of each
	// request (indexStaging).
	copied map[string]int32
	lots   map[string]zoneLot // lot rows this transaction decoded or wrote, by path
	delta  int64              // what the update's record rows change in usage
}

// txReader retains the first I/O error, including when a value/exists helper
// cannot return it. A transaction never treats failed reads as committed absence.
type txReader struct {
	zonelog.Reader
	err error
}

func (r *txReader) Get(collection, location, id string) (zonelog.Record, bool, error) {
	if r.err != nil {
		return zonelog.Record{}, false, r.err
	}
	pointReads.Add(1)
	record, ok, err := r.Reader.Get(collection, location, id)
	r.err = err
	return record, ok, err
}

func (r *txReader) ScanWith(collection, zone string, after *zonelog.Record, ids *zonelog.IDRange, load func(zonelog.Record) bool, visit func(zonelog.Record, int) bool) error {
	if r.err != nil {
		return r.err
	}
	rangeScans.Add(1)
	err := r.Reader.ScanWith(collection, zone, after, ids, load, visit)
	// A callback can perform another read on this transaction. Its failure
	// must survive a successful outer scan.
	if r.err == nil {
		r.err = err
	}
	return r.err
}

func (s *Store) transaction(reader zonelog.Reader, writable bool) *Tx {
	tx := newReadTx(s.db, reader)
	tx.owner, tx.writable = s, writable
	return tx
}

func newTx(db *zonelog.Store, writable bool) *Tx {
	tx := newReadTx(db, db)
	tx.writable = writable
	return tx
}

func newReadTx(db *zonelog.Store, reader zonelog.Reader) *Tx {
	// pending and below stay nil until the first set: most transactions
	// only read, and reads of a nil map are what an empty one answers.
	tx := &Tx{db: db, version: reader.Seq()}
	tx.ownReader.Reader = reader
	tx.reader = &tx.ownReader
	return tx
}

// run keeps an I/O error distinct from any refusal derived from a missing row.
func (tx *Tx) run(fn func(*Tx) error) error {
	err := fn(tx)
	if tx.reader.err != nil {
		return tx.reader.err
	}
	return err
}

// ID changes with every commit that changed anything (zonelog's seq).
func (tx *Tx) ID() int { return int(tx.version) }

// controlRows are the control rows the update wrote, for its batch, in path
// order. They are counted rows: control rows are live data, and zonelog
// takes rows that are neither live nor counted for waste to merge away.
func (tx *Tx) controlRows() []zonelog.Record {
	out := make([]zonelog.Record, 0, len(tx.pending))
	for _, k := range sortedKeys(tx.pending) {
		p := tx.pending[k]
		out = append(out, zonelog.Record{Collection: kvCollection, Location: "@", ID: k, Payload: p, Deleted: p == nil})
	}
	return out
}

// sortedKeys is the keys of m in order, in one allocation.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// get is the payload at path: the update's own write, else the committed row.
func (tx *Tx) get(path string) ([]byte, error) {
	if p, ok := tx.pending[path]; ok {
		return p, nil
	}
	r, ok, err := tx.reader.Get(kvCollection, "@", path)
	if err != nil || !ok {
		return nil, err
	}
	return r.Payload, nil
}

func (tx *Tx) set(path string, payload []byte) error {
	if tx.reader.err != nil {
		return tx.reader.err
	}
	if !tx.writable {
		return errKVReadOnly
	}
	if tx.pending == nil {
		tx.pending, tx.below = map[string][]byte{}, map[string]int{}
	}
	if was, is := tx.pending[path] != nil, payload != nil; was != is {
		d := -1
		if is {
			d = 1
		}
		for i := 0; i+1 < len(path); i++ {
			if path[i] == 0 && path[i+1] == 0 && i+2 < len(path) {
				tx.below[path[:i+2]] += d
			}
			if path[i] == 0 {
				i++ // a terminator or an escaped 0x00
			}
		}
	}
	if _, seen := tx.pending[path]; !seen {
		tx.recent = append(tx.recent, path)
		if len(tx.recent) >= pendingMerge {
			tx.mergeRecent()
		}
	}
	tx.pending[path] = payload
	return nil
}

// pendingMerge is how many new paths wait in recent before a merge.
const pendingMerge = 256

func (tx *Tx) mergeRecent() {
	slices.Sort(tx.recent)
	merged := make([]string, 0, len(tx.sorted)+len(tx.recent))
	i, j := 0, 0
	for i < len(tx.sorted) || j < len(tx.recent) {
		if j == len(tx.recent) || i < len(tx.sorted) && tx.sorted[i] < tx.recent[j] {
			merged = append(merged, tx.sorted[i])
			i++
		} else {
			merged = append(merged, tx.recent[j])
			j++
		}
	}
	tx.sorted, tx.recent = merged, tx.recent[:0]
}

// rows calls visit with every row whose path starts with prefix and is not
// below from, in order: the committed rows under the update's own writes.
func (tx *Tx) rows(prefix, from string, visit func(path string, payload []byte) bool) error {
	return tx.scanRows(prefix, from, true, visit)
}

// scanRows can discard values when only row identities are needed. The sizes
// retain exact capacity accounting without fetching the deleted payloads again.
func (tx *Tx) scanRows(prefix, from string, payloads bool, visit func(path string, payload []byte) bool) error {
	from = max(from, prefix)
	var mine []string
	// The paths with prefix are contiguous in sorted, from from on.
	at, _ := slices.BinarySearch(tx.sorted, from)
	for ; at < len(tx.sorted) && strings.HasPrefix(tx.sorted[at], prefix); at++ {
		mine = append(mine, tx.sorted[at])
	}
	for _, k := range tx.recent {
		if k >= from && strings.HasPrefix(k, prefix) {
			mine = append(mine, k)
		}
	}
	slices.Sort(mine)
	stop := false
	emit := func(before string) { // the update's rows before before ("": all)
		for len(mine) > 0 && !stop && (before == "" || mine[0] < before) {
			if p := tx.pending[mine[0]]; p != nil {
				stop = !visit(mine[0], p)
			}
			mine = mine[1:]
		}
	}
	all := func(r zonelog.Record) bool {
		_, shadowed := tx.pending[r.ID]
		return payloads && !shadowed
	}
	ids := zonelog.IDRange{Prefix: prefix, From: from}
	err := scanPaged(tx.reader, kvCollection, "@", nil, &ids, all, func(r zonelog.Record, size int) bool {
		if tx.writable {
			if tx.sizes == nil {
				tx.sizes = map[string]int{}
			}
			tx.sizes[r.ID] = size
		}
		if emit(r.ID); stop {
			return false
		}
		if _, ok := tx.pending[r.ID]; ok {
			return true // the update's version, emitted in order
		}
		stop = !visit(r.ID, r.Payload)
		return !stop
	})
	emit("")
	return err
}

// committedSize is the payload length of the row at path before this update,
// and whether there was one. An update remembers the answer: the usage and
// the staging index both ask it of every row the update changed, and the
// committed rows do not change under the writer.
func (tx *Tx) committedSize(path string) (int, bool) {
	if size, ok := tx.sizes[path]; ok {
		return size, true
	}
	if tx.absent[path] {
		return 0, false
	}
	r, ok, err := tx.reader.Get(kvCollection, "@", path)
	ok = ok && err == nil && len(r.Payload) > 0
	switch {
	case !tx.writable || err != nil:
	case ok:
		if tx.sizes == nil {
			tx.sizes = map[string]int{}
		}
		tx.sizes[path] = len(r.Payload)
	default:
		if tx.absent == nil {
			tx.absent = map[string]bool{}
		}
		tx.absent[path] = true
	}
	return len(r.Payload), ok
}

// key is the path of a control row: its elements, each escaped and ended.
func key(elems ...[]byte) string {
	var b strings.Builder
	for _, e := range elems {
		b.WriteString(element(e))
	}
	return b.String()
}

// split is the elements of a path.
func split(path string) [][]byte {
	var out [][]byte
	for path != "" {
		i := 0
		for i+1 < len(path) && !(path[i] == 0 && path[i+1] == 0) {
			if path[i] == 0 {
				i++ // an escaped 0x00
			}
			i++
		}
		end := min(i+2, len(path))
		out = append(out, unescape(path[:end]))
		path = path[end:]
	}
	return out
}

// value is the value of the row at path, nil for none.
func (tx *Tx) value(path string) []byte {
	p, err := tx.get(path)
	if err != nil || len(p) == 0 {
		return nil
	}
	return p[1:]
}

func (tx *Tx) put(path string, value []byte) error {
	return tx.set(path, append([]byte{'v'}, value...))
}

// putCounted puts the row name at path and adds to used what that changes
// in usage: a new row its name and value, a replaced one the difference.
func (tx *Tx) putCounted(path string, name, value []byte, used *int64) error {
	if old := tx.value(path); old == nil {
		*used += int64(len(name) + len(value))
	} else {
		*used += int64(len(value) - len(old))
	}
	return tx.put(path, value)
}

func (tx *Tx) del(path string) error {
	if _, pending := tx.pending[path]; !pending {
		if _, exists := tx.sizes[path]; exists {
			return tx.set(path, nil)
		}
	}
	if p, err := tx.get(path); err != nil || p == nil {
		return err
	}
	return tx.set(path, nil)
}

// each visits the values below prefix from from on, in order.
func (tx *Tx) each(prefix, from string, visit func(path string, value []byte) bool) error {
	return tx.rows(prefix, from, func(path string, payload []byte) bool {
		return path == prefix || visit(path, payload[1:])
	})
}

// any reports whether a row is below prefix, a path.
func (tx *Tx) any(prefix string) bool {
	if tx.below[prefix] > 0 {
		return true
	}
	found := false
	// The exact prefix may hold a reservation. Its first possible strict
	// descendant is prefix followed by a zero byte.
	ids := zonelog.IDRange{Prefix: prefix, From: prefix + "\x00"}
	tx.reader.ScanWith(kvCollection, "@", nil, &ids, func(zonelog.Record) bool { return false }, func(r zonelog.Record, _ int) bool {
		_, gone := tx.pending[r.ID] // deleted: a row put here counts in below
		found = !gone
		return gone
	})
	return found
}

// drop deletes the rows at and below prefix.
func (tx *Tx) drop(prefix string) error {
	var gone []string
	err := tx.scanRows(prefix, "", false, func(path string, _ []byte) bool { gone = append(gone, path); return true })
	for _, path := range gone {
		if err == nil {
			err = tx.set(path, nil)
		}
	}
	return err
}

// size is what the rows below prefix take: each row's last element and its
// value (bucket names are not counted: see usageKey).
func (tx *Tx) size(prefix string) (int64, error) {
	var used int64
	err := tx.rows(prefix, "", func(path string, payload []byte) bool {
		if path == prefix {
			return true // a reservation at prefix is not below it
		}
		elems := split(path[len(prefix):])
		used += int64(len(elems[len(elems)-1]) + len(payload) - 1)
		return true
	})
	return used, err
}

func escape(name []byte) string {
	return strings.ReplaceAll(string(name), "\x00", "\x00\x01")
}

func element(name []byte) string { return escape(name) + "\x00\x00" }

func unescape(elem string) []byte {
	return []byte(strings.ReplaceAll(strings.TrimSuffix(elem, "\x00\x00"), "\x00\x01", "\x00"))
}

func zoneBytes(z tree.ZoneKey) []byte { return []byte(sealKey(z)) }

func number(n uint64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], n)
	return out[:]
}

// meta is the path of a row of the meta bucket.
func meta(name []byte) string { return key(metaBucket, name) }

// number8 reads a number written by number at path; anything else is zero.
func (tx *Tx) number8(path string) uint64 {
	if v := tx.value(path); len(v) == 8 {
		return binary.BigEndian.Uint64(v)
	}
	return 0
}

func (tx *Tx) putJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.put(path, raw)
}

func (tx *Tx) readJSON(path string, v any) error {
	raw := tx.value(path)
	if raw == nil {
		return fmt.Errorf("missing durable field %s", split(path)[len(split(path))-1])
	}
	return json.Unmarshal(raw, v)
}

func recordZone(r Record) (tree.ZoneKey, error) {
	if r.ID == "" {
		return tree.ZoneKey{}, fmt.Errorf("record identity required")
	}
	return tree.NewZoneKey(r.Collection, r.Location)
}
