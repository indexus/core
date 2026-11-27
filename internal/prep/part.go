// Package prep is the file a publisher hands a node to install records in
// bulk: parts of records sorted in the store's key order, as columns, with
// checksums, and a manifest. See docs/ingest/pipeline.md, section 5.
// Standard library and internal/tree only.
package prep

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"os"

	"github.com/indexus/go-indexus-core/internal/tree"
)

const (
	magic     = "IXP1"
	version   = 1
	headLen   = 13 // location bits (12 B) | depth (1 B)
	locLen    = 12
	maxRows   = 1 << 16
	maxBlock  = 64 << 20 // payload bytes after which a block ends
	headerLen = 16
	footerLen = 32
	indexLen  = 32
)

// Head is a location as the store sorts it: its bits, left aligned, and its
// depth in characters.
type Head [headLen]byte

// MakeHead packs a location ("@" or "" is the root).
func MakeHead(location string) (Head, error) {
	var h Head
	if location == "" || location == tree.BASE64.Root() {
		return h, nil
	}
	if err := tree.CheckLocation(location); err != nil {
		return h, err
	}
	bits, err := tree.BASE64.Decode(location)
	if err != nil {
		return h, err
	}
	copy(h[:locLen], bits)
	h[locLen] = byte(len(location))
	return h, nil
}

// Depth is the number of characters of the location.
func (h Head) Depth() int { return int(h[locLen]) }

// Location is the location string ("@" for the root).
func (h Head) Location() string {
	if h.Depth() == 0 {
		return tree.BASE64.Root()
	}
	return tree.BASE64.Encode(h[:locLen])[:h.Depth()]
}

// Prefix is the zone of depth d that holds h (d <= h.Depth()).
func (h Head) Prefix(d int) Head {
	var p Head
	for bit := 0; bit < 6*d; bit++ {
		p[bit/8] |= h[bit/8] & (0x80 >> (bit % 8))
	}
	p[locLen] = byte(d)
	return p
}

// Row is one record. ID, Metrics and Payload may alias the reader's buffers
// until the next row.
type Row struct {
	Head    Head
	ID      []byte
	Weight  float64
	Metrics []float64
	Payload []byte
}

func compareKey(h1 Head, id1 []byte, h2 Head, id2 []byte) int {
	if c := bytes.Compare(h1[:], h2[:]); c != 0 {
		return c
	}
	return bytes.Compare(id1, id2)
}

// Writer writes one part. Rows must come in strictly increasing key order.
type Writer struct {
	f        *os.File
	w        *bufio.Writer
	sum      hash.Hash
	m        int
	payloads bool
	off      uint64
	rows     uint64
	index    []byte
	blocks   uint32

	heads   []Head
	ids     []byte
	idEnd   []uint32
	payEnd  []uint64
	weights []float64
	metrics [][]float64
	pay     []byte

	first, last     Head
	firstID, lastID []byte
}

// Create starts a part of m metrics; payloads says whether rows carry them.
func Create(path string, m int, payloads bool) (*Writer, error) {
	if m < 0 || m > math.MaxUint16 {
		return nil, fmt.Errorf("prep: %d metrics", m)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	pw := &Writer{f: f, sum: sha256.New(), m: m, payloads: payloads, metrics: make([][]float64, m)}
	pw.w = bufio.NewWriterSize(io.MultiWriter(f, pw.sum), 1<<20)
	h := make([]byte, headerLen)
	copy(h, magic)
	binary.LittleEndian.PutUint16(h[4:], version)
	binary.LittleEndian.PutUint16(h[6:], uint16(m))
	if payloads {
		h[8] = 1
	}
	return pw, pw.write(h)
}

func (pw *Writer) write(b []byte) error {
	_, err := pw.w.Write(b)
	pw.off += uint64(len(b))
	return err
}

// Add appends a row.
func (pw *Writer) Add(r Row) error {
	if len(r.Metrics) != pw.m {
		return fmt.Errorf("prep: %s: %d metrics, want %d", r.Head.Location(), len(r.Metrics), pw.m)
	}
	if len(r.Payload) > 0 && !pw.payloads {
		return errors.New("prep: payload in a part without payloads")
	}
	if pw.rows+uint64(len(pw.heads)) > 0 && compareKey(pw.last, pw.lastID, r.Head, r.ID) >= 0 {
		return fmt.Errorf("prep: %s/%s is not after %s/%s", r.Head.Location(), r.ID, pw.last.Location(), pw.lastID)
	}
	for _, v := range r.Metrics {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("prep: %s: metric %v", r.Head.Location(), v)
		}
	}
	if pw.rows+uint64(len(pw.heads)) == 0 {
		pw.first, pw.firstID = r.Head, append([]byte(nil), r.ID...)
	}
	pw.last, pw.lastID = r.Head, append(pw.lastID[:0], r.ID...)
	pw.heads = append(pw.heads, r.Head)
	pw.ids = append(pw.ids, r.ID...)
	pw.idEnd = append(pw.idEnd, uint32(len(pw.ids)))
	pw.pay = append(pw.pay, r.Payload...)
	pw.payEnd = append(pw.payEnd, uint64(len(pw.pay)))
	pw.weights = append(pw.weights, r.Weight)
	for j, v := range r.Metrics {
		pw.metrics[j] = append(pw.metrics[j], v)
	}
	if len(pw.heads) == maxRows || len(pw.pay) >= maxBlock {
		return pw.flush()
	}
	return nil
}

func pad8(b []byte) []byte {
	for len(b)%8 != 0 {
		b = append(b, 0)
	}
	return b
}

func (pw *Writer) flush() error {
	n := len(pw.heads)
	if n == 0 {
		return nil
	}
	b := make([]byte, 16, 16+n*(headLen+4+8+8*(1+pw.m))+len(pw.ids)+len(pw.pay)+32)
	binary.LittleEndian.PutUint32(b, uint32(n))
	binary.LittleEndian.PutUint32(b[4:], uint32(len(pw.ids)))
	binary.LittleEndian.PutUint64(b[8:], uint64(len(pw.pay)))
	for _, h := range pw.heads {
		b = append(b, h[:]...)
	}
	b = pad8(b)
	for _, e := range pw.idEnd {
		b = binary.LittleEndian.AppendUint32(b, e)
	}
	b = pad8(b)
	if pw.payloads {
		for _, e := range pw.payEnd {
			b = binary.LittleEndian.AppendUint64(b, e)
		}
	}
	for _, v := range pw.weights {
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
	}
	for _, col := range pw.metrics {
		for _, v := range col {
			b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
		}
	}
	b = pad8(append(b, pw.ids...))
	b = pad8(append(b, pw.pay...))
	b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
	b = binary.LittleEndian.AppendUint32(b, 0)

	ix := make([]byte, indexLen)
	copy(ix, pw.heads[0][:])
	binary.LittleEndian.PutUint64(ix[16:], pw.off)
	binary.LittleEndian.PutUint32(ix[24:], uint32(len(b)))
	binary.LittleEndian.PutUint32(ix[28:], uint32(n))
	pw.index = append(pw.index, ix...)
	pw.blocks++
	pw.rows += uint64(n)

	pw.heads, pw.ids, pw.idEnd, pw.payEnd, pw.weights, pw.pay = pw.heads[:0], pw.ids[:0], pw.idEnd[:0], pw.payEnd[:0], pw.weights[:0], pw.pay[:0]
	for j := range pw.metrics {
		pw.metrics[j] = pw.metrics[j][:0]
	}
	return pw.write(b)
}

// PartInfo is what the manifest says of a part.
type PartInfo struct {
	File    string `json:"file"`
	Records uint64 `json:"records"`
	Bytes   uint64 `json:"bytes"`
	First   string `json:"first"`
	Last    string `json:"last"`
	SHA256  string `json:"sha256"`
}

func keyString(h Head, id []byte) string {
	return h.Location() + "/" + string(id)
}

// Close writes the index and the footer.
func (pw *Writer) Close() (PartInfo, error) {
	err := pw.flush()
	at := pw.off
	if err == nil {
		err = pw.write(pw.index)
	}
	foot := make([]byte, footerLen)
	binary.LittleEndian.PutUint64(foot, at)
	binary.LittleEndian.PutUint64(foot[8:], pw.rows)
	binary.LittleEndian.PutUint32(foot[16:], pw.blocks)
	binary.LittleEndian.PutUint32(foot[20:], crc32.ChecksumIEEE(pw.index))
	copy(foot[24:], magic)
	if err == nil {
		err = pw.write(foot)
	}
	if err == nil {
		err = pw.w.Flush()
	}
	if cerr := pw.f.Close(); err == nil {
		err = cerr
	}
	info := PartInfo{Records: pw.rows, Bytes: pw.off, SHA256: fmt.Sprintf("%x", pw.sum.Sum(nil))}
	if pw.rows > 0 {
		info.First, info.Last = keyString(pw.first, pw.firstID), keyString(pw.last, pw.lastID)
	}
	return info, err
}

// Reader reads a part block by block.
type Reader struct {
	r        io.ReaderAt
	M        int
	Payloads bool
	Rows     uint64
	blocks   []blockRef
}

type blockRef struct {
	first   Head
	off     uint64
	size, n uint32
}

// Open checks the header, the footer and the index of a part.
func Open(r io.ReaderAt, size int64) (*Reader, error) {
	if size < headerLen+footerLen {
		return nil, errors.New("prep: part too short")
	}
	h, foot := make([]byte, headerLen), make([]byte, footerLen)
	if _, err := r.ReadAt(h, 0); err != nil {
		return nil, err
	}
	if _, err := r.ReadAt(foot, size-footerLen); err != nil {
		return nil, err
	}
	if string(h[:4]) != magic || string(foot[24:28]) != magic {
		return nil, errors.New("prep: not an IXP1 part")
	}
	if v := binary.LittleEndian.Uint16(h[4:]); v != version {
		return nil, fmt.Errorf("prep: version %d", v)
	}
	pr := &Reader{r: r, M: int(binary.LittleEndian.Uint16(h[6:])), Payloads: h[8]&1 != 0, Rows: binary.LittleEndian.Uint64(foot[8:])}
	at, nb := binary.LittleEndian.Uint64(foot), binary.LittleEndian.Uint32(foot[16:])
	if at > uint64(size) || uint64(size)-footerLen-at != uint64(nb)*indexLen {
		return nil, errors.New("prep: bad index bounds")
	}
	ix := make([]byte, int(nb)*indexLen)
	if _, err := r.ReadAt(ix, int64(at)); err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(ix) != binary.LittleEndian.Uint32(foot[20:]) {
		return nil, errors.New("prep: index CRC")
	}
	var total uint64
	for i := range int(nb) {
		e := ix[i*indexLen:]
		var b blockRef
		copy(b.first[:], e[:headLen])
		b.off, b.size, b.n = binary.LittleEndian.Uint64(e[16:]), binary.LittleEndian.Uint32(e[24:]), binary.LittleEndian.Uint32(e[28:])
		if b.off+uint64(b.size) > at {
			return nil, errors.New("prep: block past the index")
		}
		pr.blocks = append(pr.blocks, b)
		total += uint64(b.n)
	}
	if total != pr.Rows {
		return nil, errors.New("prep: row count")
	}
	return pr, nil
}

// OpenFile opens a part on disk; close the returned file when done.
func OpenFile(path string) (*Reader, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err == nil {
		var pr *Reader
		if pr, err = Open(f, st.Size()); err == nil {
			return pr, f, nil
		}
	}
	f.Close()
	return nil, nil, err
}

// Blocks is the number of blocks; BlockHead the first head of block i, for
// a binary search of an owner boundary.
func (pr *Reader) Blocks() int { return len(pr.blocks) }

// Each calls visit for every row in order, checking block CRCs and the order
// of keys. The row's slices are valid until visit returns.
func (pr *Reader) Each(visit func(Row) error) error {
	var prev Head
	var prevID []byte
	seen := false
	for _, b := range pr.blocks {
		if err := pr.block(b, func(r Row) error {
			if seen && compareKey(prev, prevID, r.Head, r.ID) >= 0 {
				return fmt.Errorf("prep: %s/%s out of order", r.Head.Location(), r.ID)
			}
			seen, prev, prevID = true, r.Head, append(prevID[:0], r.ID...)
			return visit(r)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (pr *Reader) block(b blockRef, visit func(Row) error) error {
	raw := make([]byte, b.size)
	if _, err := pr.r.ReadAt(raw, int64(b.off)); err != nil {
		return err
	}
	if b.size < 24 || crc32.ChecksumIEEE(raw[:b.size-8]) != binary.LittleEndian.Uint32(raw[b.size-8:]) {
		return errors.New("prep: block CRC")
	}
	n := int(binary.LittleEndian.Uint32(raw))
	idBytes, payBytes := int(binary.LittleEndian.Uint32(raw[4:])), int(binary.LittleEndian.Uint64(raw[8:]))
	if n != int(b.n) {
		return errors.New("prep: block rows")
	}
	at := 16
	take := func(k int) ([]byte, error) {
		if k < 0 || at+k > len(raw)-8 {
			return nil, errors.New("prep: block too short")
		}
		s := raw[at : at+k]
		at += k
		at += (8 - at%8) % 8
		return s, nil
	}
	heads, err := take(n * headLen)
	if err != nil {
		return err
	}
	idEnd, err := take(4 * n)
	if err != nil {
		return err
	}
	var payEnd []byte
	if pr.Payloads {
		if payEnd, err = take(8 * n); err != nil {
			return err
		}
	}
	nums, err := take(8 * n * (1 + pr.M))
	if err != nil {
		return err
	}
	ids, err := take(idBytes)
	if err != nil {
		return err
	}
	pay, err := take(payBytes)
	if err != nil {
		return err
	}
	metrics := make([]float64, pr.M)
	var r Row
	r.Metrics = metrics
	idAt, payAt := 0, 0
	for i := range n {
		copy(r.Head[:], heads[i*headLen:])
		e := int(binary.LittleEndian.Uint32(idEnd[4*i:]))
		if e < idAt || e > len(ids) {
			return errors.New("prep: id offsets")
		}
		r.ID, idAt = ids[idAt:e], e
		r.Payload = nil
		if pr.Payloads {
			e := int(binary.LittleEndian.Uint64(payEnd[8*i:]))
			if e < payAt || e > len(pay) {
				return errors.New("prep: payload offsets")
			}
			r.Payload, payAt = pay[payAt:e], e
		}
		r.Weight = math.Float64frombits(binary.LittleEndian.Uint64(nums[8*i:]))
		for j := range pr.M {
			metrics[j] = math.Float64frombits(binary.LittleEndian.Uint64(nums[8*(n*(1+j)+i):]))
		}
		if err := visit(r); err != nil {
			return err
		}
	}
	return nil
}
