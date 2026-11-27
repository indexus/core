package zonelog

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

// MaxMetrics is the most metrics a row holds: an embedding of a few thousand
// dimensions (rows past 255 are written IXR2 and in wide pages).
const MaxMetrics = 4096

// Record is one user record. Weight 0 is read as 1: a plain record counts
// once, a pre-aggregated one carries its weight (IXR1).
type Record struct {
	Collection string
	Location   string
	ID         string
	Weight     float64
	Metrics    []float64
	Payload    []byte
	Deleted    bool
	// Uncounted keeps a record for gets and scans and out of every sum: a
	// caller's tombstone, say. It has no weight and no metrics.
	Uncounted bool
	// Opaque says the payload is the caller's own encoding of the record:
	// ScanWith hands it whatever it was asked to load.
	Opaque bool
	// Binary says the payload is raw bytes, not a JSON value.
	Binary bool
}

const (
	flagDeleted = 1 << iota
	// A retraction row cancels the sums of an older version of its key that
	// lives in an older segment. Its weight and metrics are stored negated
	// and its seq is the seq of the version it cancels.
	flagRetract
	// A receipt row remembered a request whose write was replaced (earlier
	// releases). It is in no sum and no scan.
	flagReceipt
	// An uncounted row is live for reads and adds nothing but a row to sums;
	// so does its retraction.
	flagUncounted
	// An opaque row is a live record whose payload the caller encodes.
	flagOpaque
	// A binary row's payload is raw bytes. Like opaque it says what the
	// payload is, and counts as the row it is on.
	flagBinary
)

// payloadFlags describe a payload and change no count.
const payloadFlags = flagOpaque | flagBinary

// row is a record in the store's own shape. A memtable row carries its
// payload; a row read from a segment carries where the payload is.
type row struct {
	key     []byte
	flags   byte
	seq     uint64
	req     uint64 // on disk, always 0: the hash of a request id of earlier releases
	weight  float64
	metrics []float64
	payload []byte

	// Memtable only. old is the retraction of the version on disk; below is
	// the version this row replaced in the memtable, kept for readers until
	// this row is synced.
	old, below *row

	// Where the payload is: bytes [voff, voff+vlen) of payload file val.
	val  uint64
	voff uint64
	vlen uint32
}

func (r *row) live() bool { return r.flags&^(flagUncounted|payloadFlags) == 0 }

// at returns the newest version of a memtable row that readers may see.
func (r *row) at(visible uint64) *row {
	for r != nil && r.seq > visible {
		r = r.below
	}
	return r
}

// retraction is the row that subtracts r from every sum.
func (r *row) retraction() *row {
	out := &row{key: r.key, flags: flagRetract | r.flags&(flagUncounted|payloadFlags), seq: r.seq, weight: -r.weight, metrics: make([]float64, len(r.metrics))}
	for i, v := range r.metrics {
		out.metrics[i] = -v
	}
	return out
}

// sums is [rows, count, weight, metrics...]. Every row adds to it, so the
// sum of a key range is the sum of its rows in every segment and in the
// memtable, with no lookup of which version wins. rows counts every row;
// when it equals count, every row of the range is a live record.
//
// Each column is a Neumaier pair, value then compensation (s[2i], s[2i+1]):
// a retraction cancels the row it retracts to the last bit, and 1e16 + 1 -
// 1e16 is 1, in whatever order rows, pages and segments are added.
type sums []float64

func (s *sums) grow(cols int) {
	for len(*s) < 2*cols {
		*s = append(*s, 0)
	}
}

// at is column i: its value plus its compensation.
func (s sums) at(i int) float64 {
	if 2*i+1 >= len(s) {
		return 0
	}
	return s[2*i] + s[2*i+1]
}

// cols is how many columns s holds.
func (s sums) cols() int { return len(s) / 2 }

// put adds x to column i (Neumaier).
func (s sums) put(i int, x float64) {
	v := s[2*i]
	t := v + x
	if math.Abs(v) >= math.Abs(x) {
		s[2*i+1] += (v - t) + x
	} else {
		s[2*i+1] += (x - t) + v
	}
	s[2*i] = t
}

func (s *sums) addRow(r *row) {
	s.grow(3 + len(r.metrics))
	rows, count := countOf(r.flags)
	s.put(0, rows)
	s.put(1, count)
	s.put(2, r.weight)
	for i, v := range r.metrics {
		s.put(3+i, v)
	}
}

func (s *sums) add(o sums) {
	s.grow(o.cols())
	for i := range o.cols() {
		s.put(i, o[2*i])
		(*s)[2*i+1] += o[2*i+1]
	}
}

// countOf is what a row adds to the rows and to the live records of a sum.
func countOf(flags byte) (rows, count float64) {
	switch flags &^ payloadFlags {
	case 0:
		return 1, 1
	case flagRetract:
		return 1, -1
	case flagReceipt:
		return 0, 0
	}
	return 1, 0 // tombstones, uncounted rows and their retractions
}

// appendValue writes the value of a row: version, flags, metric count,
// payload length, weight, metrics, payload. IXR1 (version 1) counts metrics
// on one byte then a zero byte; IXR2 (version 2, only for rows of more than
// 255 metrics) counts them on two bytes. Rows that fit IXR1 keep it, so the
// bytes of existing collections do not change.
func appendValue(b []byte, r *row) []byte {
	if m := len(r.metrics); m > 255 {
		b = append(b, 2, r.flags, byte(m), byte(m>>8))
	} else {
		b = append(b, 1, r.flags, byte(m), 0)
	}
	b = binary.LittleEndian.AppendUint32(b, uint32(len(r.payload)))
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(r.weight))
	for _, v := range r.metrics {
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
	}
	return append(b, r.payload...)
}

// readValue is the inverse of appendValue; it returns the bytes it used, or
// 0 when b is short or not version 1 or 2.
func readValue(b []byte, r *row) int {
	if len(b) < 16 || b[0] != 1 && b[0] != 2 {
		return 0
	}
	m, p := int(b[2]), int(binary.LittleEndian.Uint32(b[4:]))
	if b[0] == 2 {
		m = int(binary.LittleEndian.Uint16(b[2:]))
	}
	if len(b) < 16+8*m+p {
		return 0
	}
	r.flags = b[1]
	r.weight = math.Float64frombits(binary.LittleEndian.Uint64(b[8:]))
	r.metrics = make([]float64, m)
	for i := range m {
		r.metrics[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[16+8*i:]))
	}
	if p > 0 {
		r.payload = append([]byte(nil), b[16+8*m:16+8*m+p]...)
	}
	return 16 + 8*m + p
}

func hash64(parts ...[]byte) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum64()
}

func recordWithCollection(collection string, r *row) Record {
	return Record{
		Collection: collection,
		Location:   locString(r.key[2:2+locLen], int(r.key[headLen-1])),
		ID:         string(r.key[headLen:]),
		Weight:     r.weight,
		Metrics:    r.metrics,
		Payload:    r.payload,
		Deleted:    r.flags&flagDeleted != 0,
		Uncounted:  r.flags&flagUncounted != 0,
		Opaque:     r.flags&flagOpaque != 0,
		Binary:     r.flags&flagBinary != 0,
	}
}
