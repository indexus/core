package zonelog

import (
	"bufio"
	"bytes"
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"math"
	"math/bits"
	"slices"
	"sort"
	"sync"
)

// A segment NNN.seg is immutable: pages of rows laid out as columns, then
// the page index, a Bloom filter of the keys, the payload files it points
// into, and a footer. Payloads are in payload files NNN.val, written with
// the segment of the same number. A merge copies small payloads into its own
// payload file and keeps pointing at the large ones, so those are written
// once.
//
//	page    u16 rows n | u8 metric columns m | u8 1 when rows have payloads | u32 id bytes | 8 × 0
//	        [n][15]u8 key heads | [n]u8 flags | [n]u8 metric count | [n]u16 id end
//	        A wide page (more than 255 metric columns) sets byte 8 of its
//	        header to 1, counts its columns on u16 at bytes 9-10 and each
//	        row's metrics on [n]u16; a segment with a wide page ends "ZLS4".
//	        A page of 64 metric columns or more whose every metric is a
//	        float32 exactly (embeddings) sets byte 11 to 1 and stores them
//	        as [m][n]f32: half the bytes, the same values; its segment ends
//	        "ZLS4" too.
//	        [n]u64 seq | [n]u64 request | [n]f64 weight
//	        with payloads: [n]u32 payload file | [n]u64 offset | [n]u32 bytes
//	        [m][n]f64 metrics (or f32, see above) | id bytes | u32 CRC
//	tail    u32 pages | per page: u32 n, first key | u32 n, last key |
//	        u64 offset | u32 bytes | u32 n, [n]f64 sums
//	        u32 n, filter | u32 files | per file: u64 file, u64 bytes |
//	        u64 rows | u64 highest seq (all little endian)
//	footer  u64 tail offset | u32 CRC of the tail | "ZLS3" (or "ZLS4", see above)
//
// An open segment keeps little in memory: per run of superPages pages its
// first key, its sums and where its index entries are; per bloomChunk bytes
// of the filter a CRC. The index entries and the filter are read through the
// page cache, like the pages, so the memory of a store does not grow with
// its records.
const (
	pageRows   = 256
	superPages = 64
	pageHeader = 16
	footerLen  = 16
	bloomBits  = 10
	bloomK     = 6
	bloomChunk = 4096
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var errDamagedIndex = errors.New("damaged index")

// pageRef is the index entry of a page.
type pageRef struct {
	First, Last []byte
	Off         uint64
	Size        uint32
	Sum         sums
}

// block is what a segment keeps in memory of a run of superPages pages.
type block struct {
	first []byte // first key of the run
	off   int64  // its index entries in the file
	size  uint32
	crc   uint32
	sum   sums // sums of the run
}

type segment struct {
	id           uint64
	f            File
	bytes        int64
	pages        int
	blocks       []block
	last         []byte           // the last key
	bloomAt      int64            // the filter in the file
	bloomLen     int              // its bytes
	chunks       []uint32         // CRC of each bloomChunk bytes of it
	Files        map[uint64]int64 // payload bytes this segment points at, by file
	Rows, MaxSeq uint64
}

func segName(id uint64) string { return fmt.Sprintf("%08d.seg", id) }
func valName(id uint64) string { return fmt.Sprintf("%08d.val", id) }

type page struct {
	raw                                       []byte
	wide                                      bool // metric counts on u16
	f32                                       bool // metrics stored as float32
	n, m                                      int
	flags, nmet, idEnd, seq, req, weight, val int
	met, ids                                  int
}

// f32Columns is the narrowest page that may store its metrics as float32:
// vectors, not the few columns of an ordinary collection, whose pages keep
// the format older releases read.
const f32Columns = 64

// layout places the columns of a page of n rows and m metric columns, of
// float32 metrics when f32.
func layout(n, m int, payloads, f32 bool) *page {
	p := &page{n: n, m: m, wide: m > 255, f32: f32, flags: pageHeader + n*headLen}
	nm := 1
	if p.wide {
		nm = 2
	}
	p.nmet = p.flags + n
	p.idEnd = p.nmet + nm*n
	p.seq = p.idEnd + 2*n
	p.req, p.weight, p.val = p.seq+8*n, p.seq+16*n, p.seq+24*n
	p.met = p.val
	if payloads {
		p.met += 16 * n
	}
	p.ids = p.met + p.metricBytes()*n*m
	return p
}

func (p *page) metricBytes() int {
	if p.f32 {
		return 4
	}
	return 8
}

// metric is metric column c of row r.
func (p *page) metric(c, r int) float64 {
	if p.f32 {
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(p.raw[p.met+4*(p.n*c+r):])))
	}
	return p.f64(p.met + 8*(p.n*c+r))
}

// exactF32 reports whether every metric of rows survives float32 unchanged
// (NaN payloads and signed zeros included).
func exactF32(rows []*row) bool {
	for _, r := range rows {
		for _, v := range r.metrics {
			if math.Float64bits(float64(float32(v))) != math.Float64bits(v) {
				return false
			}
		}
	}
	return true
}

func parsePage(raw []byte) (*page, error) {
	if len(raw) < pageHeader+4 || crc32.Checksum(raw[:len(raw)-4], crcTable) != binary.LittleEndian.Uint32(raw[len(raw)-4:]) {
		return nil, fmt.Errorf("zonelog: damaged page")
	}
	m := int(raw[2])
	if raw[8] == 1 {
		m = int(binary.LittleEndian.Uint16(raw[9:]))
	}
	if raw[11] > 1 || raw[11] == 1 && m < f32Columns {
		return nil, fmt.Errorf("zonelog: damaged page")
	}
	p := layout(int(binary.LittleEndian.Uint16(raw)), m, raw[3] == 1, raw[11] == 1)
	if p.wide != (raw[8] == 1) {
		return nil, fmt.Errorf("zonelog: damaged page")
	}
	p.raw = raw
	return p, nil
}

func (p *page) f64(at int) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(p.raw[at:]))
}

func (p *page) head(i int) []byte { return p.raw[pageHeader+i*headLen : pageHeader+(i+1)*headLen] }

func (p *page) id(i int) []byte {
	start := 0
	if i > 0 {
		start = int(binary.LittleEndian.Uint16(p.raw[p.idEnd+2*i-2:]))
	}
	return p.raw[p.ids+start : p.ids+int(binary.LittleEndian.Uint16(p.raw[p.idEnd+2*i:]))]
}

// search returns the first row whose key is not below key.
func (p *page) search(key []byte) int {
	return sort.Search(p.n, func(i int) bool {
		c := bytes.Compare(p.head(i), key[:headLen])
		if c == 0 {
			c = bytes.Compare(p.id(i), key[headLen:])
		}
		return c >= 0
	})
}

// sum adds rows [i, j) to out, column by column.
func (p *page) sum(i, j int, out *sums) {
	out.grow(3 + p.m)
	s := *out
	for r := i; r < j; r++ {
		rows, count := countOf(p.raw[p.flags+r])
		s.put(0, rows)
		s.put(1, count)
		s.put(2, p.f64(p.weight+8*r))
	}
	for c := range p.m {
		for r := i; r < j; r++ {
			s.put(3+c, p.metric(c, r))
		}
	}
}

// rowMetrics is how many metrics row i has.
func (p *page) rowMetrics(i int) int {
	if p.wide {
		return int(binary.LittleEndian.Uint16(p.raw[p.nmet+2*i:]))
	}
	return int(p.raw[p.nmet+i])
}

// row copies row i out of the page, without its payload.
func (p *page) row(i int) *row {
	r := &row{key: append(append(make([]byte, 0, headLen+16), p.head(i)...), p.id(i)...), flags: p.raw[p.flags+i]}
	r.seq = binary.LittleEndian.Uint64(p.raw[p.seq+8*i:])
	r.req = binary.LittleEndian.Uint64(p.raw[p.req+8*i:])
	r.weight = p.f64(p.weight + 8*i)
	r.metrics = make([]float64, p.rowMetrics(i))
	for c := range r.metrics {
		r.metrics[c] = p.metric(c, i)
	}
	if p.met != p.val {
		r.val = uint64(binary.LittleEndian.Uint32(p.raw[p.val+4*i:]))
		r.voff = binary.LittleEndian.Uint64(p.raw[p.val+4*p.n+8*i:])
		r.vlen = binary.LittleEndian.Uint32(p.raw[p.val+12*p.n+4*i:])
	}
	return r
}

// cache keeps parsed pages up to a byte budget, least recently used out first.
type cache struct {
	mu    sync.Mutex
	limit int
	used  int
	byKey map[[2]uint64]*list.Element
	order *list.List
}

type cached struct {
	key  [2]uint64
	v    any
	size int
}

func newCache(limit int) *cache {
	return &cache{limit: limit, byKey: map[[2]uint64]*list.Element{}, order: list.New()}
}

func (g *segment) readPage(ref *pageRef) (*page, error) {
	raw := make([]byte, ref.Size)
	if _, err := g.f.ReadAt(raw, int64(ref.Off)); err != nil {
		return nil, err
	}
	return parsePage(raw)
}

// Cache keys: a page is (segment, page), an index block (segment | 1<<62,
// block), a filter chunk (segment | 1<<61, chunk), a payload frame (payload
// file | 1<<63, frame).
func (c *cache) page(g *segment, i int) (*page, error) {
	p, err := c.get([2]uint64{g.id, uint64(i)}, func() (any, int, error) {
		ref, err := g.ref(c, i)
		if err != nil {
			return nil, 0, err
		}
		p, err := g.readPage(ref)
		if err != nil {
			return nil, 0, err
		}
		return p, len(p.raw), nil
	})
	if err != nil {
		return nil, err
	}
	return p.(*page), nil
}

// get returns what is cached under key, or loads it and keeps it: a page,
// or a payload frame.
func (c *cache) get(key [2]uint64, load func() (any, int, error)) (any, error) {
	c.mu.Lock()
	if e := c.byKey[key]; e != nil {
		c.order.MoveToFront(e)
		c.mu.Unlock()
		return e.Value.(*cached).v, nil
	}
	c.mu.Unlock()
	v, size, err := load()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey[key] == nil {
		c.byKey[key] = c.order.PushFront(&cached{key, v, size})
		c.used += size
	}
	for c.used > c.limit && c.order.Len() > 1 {
		last := c.order.Remove(c.order.Back()).(*cached)
		delete(c.byKey, last.key)
		c.used -= last.size
	}
	return v, nil
}

// refs returns the index entries of block b: through the cache when c is
// given, read from the file otherwise.
func (g *segment) refs(c *cache, b int) ([]pageRef, error) {
	if c == nil {
		return g.readBlock(b)
	}
	v, err := c.get([2]uint64{g.id | 1<<62, uint64(b)}, func() (any, int, error) {
		refs, err := g.readBlock(b)
		// The bytes read (keys point into them), the sums decoded, the entries.
		return refs, 2*int(g.blocks[b].size) + 88*len(refs), err
	})
	if err != nil {
		return nil, err
	}
	return v.([]pageRef), nil
}

// readBlock reads and checks the index entries of block b. Keys point into
// the bytes read; sums share one array.
func (g *segment) readBlock(b int) ([]pageRef, error) {
	blk := g.blocks[b]
	raw := make([]byte, blk.size)
	if _, err := g.f.ReadAt(raw, blk.off); err != nil {
		return nil, err
	}
	if crc32.Checksum(raw, crcTable) != blk.crc {
		return nil, fmt.Errorf("zonelog: segment %d: %v", g.id, errDamagedIndex)
	}
	le := binary.LittleEndian
	n := min(superPages, g.pages-b*superPages)
	refs, floats := make([]pageRef, n), make([]float64, 0, len(raw)/8)
	short := false
	take := func(k uint64) []byte {
		if short = short || k > uint64(len(raw)); short {
			return nil
		}
		v := raw[:k:k]
		raw = raw[k:]
		return v
	}
	u32 := func() uint64 {
		if v := take(4); v != nil {
			return uint64(le.Uint32(v))
		}
		return 0
	}
	for i := range refs {
		ref := &refs[i]
		ref.First, ref.Last = take(u32()), take(u32())
		if v := take(12); v != nil {
			ref.Off, ref.Size = le.Uint64(v), le.Uint32(v[8:])
		}
		k, at := u32(), len(floats)
		for v := take(8 * k); len(v) > 0; v = v[8:] {
			floats = append(floats, math.Float64frombits(le.Uint64(v)))
		}
		ref.Sum = sums(floats[at:len(floats):len(floats)])
	}
	if short || len(raw) != 0 {
		return nil, fmt.Errorf("zonelog: segment %d: %v", g.id, errDamagedIndex)
	}
	return refs, nil
}

// ref returns the index entry of page i.
func (g *segment) ref(c *cache, i int) (*pageRef, error) {
	refs, err := g.refs(c, i/superPages)
	if err != nil {
		return nil, err
	}
	return &refs[i%superPages], nil
}

// pos is a row of a segment: page and row in the page.
type pos struct{ page, row int }

// seek returns the first row whose key is not below key.
func (g *segment) seek(c *cache, key []byte) (pos, error) {
	b := sort.Search(len(g.blocks), func(b int) bool { return bytes.Compare(g.blocks[b].first, key) > 0 }) - 1
	if b < 0 {
		return pos{}, nil
	}
	refs, err := g.refs(c, b)
	if err != nil {
		return pos{}, err
	}
	j := sort.Search(len(refs), func(j int) bool { return bytes.Compare(refs[j].First, key) > 0 }) - 1
	i := b*superPages + j
	if bytes.Compare(key, refs[j].Last) > 0 {
		return pos{i + 1, 0}, nil // between two pages: no page to read
	}
	p, err := c.page(g, i)
	if err != nil {
		return pos{}, err
	}
	return pos{i, p.search(key)}, nil
}

// sumRange adds the rows [a, b) to out: runs of superPages whole pages from
// their sums, other whole pages from the index, the two partial pages from
// their columns. The cost does not grow with the size of the range.
func (g *segment) sumRange(c *cache, a, b pos, out *sums) error {
	var refs []pageRef
	for a != b {
		last := a.page == b.page
		if a.row == 0 && a.page%superPages == 0 && a.page+superPages <= b.page {
			out.add(g.blocks[a.page/superPages].sum)
			a.page += superPages
			continue
		}
		if a.row == 0 && !last {
			if refs == nil || a.page%superPages == 0 {
				var err error
				if refs, err = g.refs(c, a.page/superPages); err != nil {
					return err
				}
			}
			out.add(refs[a.page%superPages].Sum)
		} else {
			p, err := c.page(g, a.page)
			if err != nil {
				return err
			}
			end := p.n
			if last {
				end = b.row
			}
			p.sum(a.row, end, out)
		}
		if last {
			break
		}
		a = pos{a.page + 1, 0}
	}
	return nil
}

// ends returns the key heads of the first and the last row of [a, b), which
// is not empty.
func (g *segment) ends(c *cache, a, b pos) (first, last []byte, err error) {
	if a.row > 0 {
		p, err := c.page(g, a.page)
		if err != nil {
			return nil, nil, err
		}
		first = p.head(a.row)
	} else {
		ref, err := g.ref(c, a.page)
		if err != nil {
			return nil, nil, err
		}
		first = ref.First[:headLen]
	}
	if b.row == 0 {
		ref, err := g.ref(c, b.page-1)
		if err != nil {
			return nil, nil, err
		}
		return first, ref.Last[:headLen], nil
	}
	p, err := c.page(g, b.page)
	if err != nil {
		return nil, nil, err
	}
	return first, p.head(b.row - 1), nil
}

// bloomSet sets the bits of hash h in a filter being written.
func bloomSet(bits []byte, h uint64) {
	n, step := uint64(len(bits))*8, h>>32|1
	for range bloomK {
		at := h % n
		bits[at/8] |= 1 << (at % 8)
		h += step
	}
}

// mayHold tests the filter, reading the chunks it needs through the cache.
func (g *segment) mayHold(c *cache, key []byte) (bool, error) {
	h := hash64(key)
	n, step := uint64(g.bloomLen)*8, h>>32|1
	var chunk []byte
	at := -1
	for range bloomK {
		bit := h % n
		if k := int(bit / 8 / bloomChunk); k != at {
			v, err := c.get([2]uint64{g.id | 1<<61, uint64(k)}, func() (any, int, error) {
				b := make([]byte, min(bloomChunk, g.bloomLen-k*bloomChunk))
				if _, err := g.f.ReadAt(b, g.bloomAt+int64(k)*bloomChunk); err != nil {
					return nil, 0, err
				}
				if crc32.Checksum(b, crcTable) != g.chunks[k] {
					return nil, 0, fmt.Errorf("zonelog: segment %d: damaged filter", g.id)
				}
				return b, len(b) + 64, nil
			})
			if err != nil {
				return false, err
			}
			chunk, at = v.([]byte), k
		}
		if chunk[bit/8%bloomChunk]&(1<<(bit%8)) == 0 {
			return false, nil
		}
		h += step
	}
	return true, nil
}

// get returns the stored row of key, or nil. A key outside the segment's
// range needs no filter: a segment of one collection is not read for
// another, nor for a receipt.
func (g *segment) get(c *cache, key []byte) (*row, error) {
	if g.pages == 0 || bytes.Compare(key, g.blocks[0].first) < 0 || bytes.Compare(key, g.last) > 0 {
		return nil, nil
	}
	if ok, err := g.mayHold(c, key); !ok || err != nil {
		return nil, err
	}
	at, err := g.seek(c, key)
	if err != nil || at.page == g.pages {
		return nil, err
	}
	p, err := c.page(g, at.page)
	if err != nil {
		return nil, err
	}
	if r := p.row(at.row); bytes.Equal(r.key, key) {
		return r, nil
	}
	return nil, nil
}

// segIter walks a segment from a position. Without a cache it reads each
// page itself: that is a merge, which must not push hot pages out.
type segIter struct {
	g    *segment
	c    *cache
	at   pos
	p    *page
	refs []pageRef // without a cache: the index entries of the current block
	cur  *row
}

func (g *segment) from(c *cache, at pos) (*segIter, error) {
	it := &segIter{g: g, c: c, at: at}
	return it, it.load()
}

func (it *segIter) row() *row { return it.cur }

func (it *segIter) load() (err error) {
	it.cur = nil
	if it.p != nil && it.at.row >= it.p.n {
		it.at, it.p = pos{it.at.page + 1, 0}, nil
	}
	if it.at.page >= it.g.pages {
		return nil
	}
	if it.p == nil {
		if it.c == nil {
			if it.refs == nil || it.at.page%superPages == 0 {
				if it.refs, err = it.g.readBlock(it.at.page / superPages); err != nil {
					return err
				}
			}
			it.p, err = it.g.readPage(&it.refs[it.at.page%superPages])
		} else {
			it.p, err = it.c.page(it.g, it.at.page)
		}
		if err != nil {
			return err
		}
	}
	it.cur = it.p.row(it.at.row)
	return nil
}

func (it *segIter) next() error {
	it.at.row++
	return it.load()
}

func openSegment(fs FS, id uint64) (*segment, error) {
	g := &segment{id: id}
	var err error
	if g.f, err = fs.Open(segName(id)); err != nil {
		return nil, err
	}
	if g.bytes, err = g.f.Size(); err == nil {
		err = g.readTail()
	}
	if err != nil {
		g.f.Close()
		return nil, fmt.Errorf("zonelog: segment %d: %v", id, err)
	}
	return g, nil
}

// tailReader reads a tail in order, with the CRC of all of it and of the
// current block.
type tailReader struct {
	r        *bufio.Reader
	off, end int64
	all, blk uint32
	buf      []byte
	err      error
}

func (t *tailReader) take(n uint64) []byte {
	if t.err == nil && n > uint64(t.end-t.off) {
		t.err = errDamagedIndex
	}
	if t.err != nil {
		return nil
	}
	if uint64(cap(t.buf)) < n {
		t.buf = make([]byte, n)
	}
	b := t.buf[:n]
	if _, err := io.ReadFull(t.r, b); err != nil {
		t.err = err
		return nil
	}
	t.all, t.blk = crc32.Update(t.all, crcTable, b), crc32.Update(t.blk, crcTable, b)
	t.off += int64(n)
	return b
}

func (t *tailReader) u32() uint64 {
	if b := t.take(4); b != nil {
		return uint64(binary.LittleEndian.Uint32(b))
	}
	return 0
}

func (t *tailReader) u64() uint64 {
	if b := t.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

// readTail reads the footer and streams the tail once: it keeps what an
// open segment keeps (see the top of this file) and checks the tail's CRC.
func (g *segment) readTail() error {
	foot := make([]byte, footerLen)
	if _, err := g.f.ReadAt(foot, max(g.bytes-footerLen, 0)); err != nil {
		return fmt.Errorf("bad footer: %v", err)
	}
	at, magic := int64(binary.LittleEndian.Uint64(foot)), string(foot[12:])
	if magic != "ZLS3" && magic != "ZLS4" {
		return fmt.Errorf("format %q", magic)
	}
	if at < 0 || at > g.bytes-footerLen {
		return fmt.Errorf("bad footer")
	}
	end := g.bytes - footerLen
	t := &tailReader{r: bufio.NewReaderSize(io.NewSectionReader(g.f, at, end-at), 1<<16), off: at, end: end}
	g.pages = int(t.u32())
	var sum sums
	closeBlock := func() {
		if n := len(g.blocks); n > 0 {
			g.blocks[n-1].size, g.blocks[n-1].crc = uint32(t.off-g.blocks[n-1].off), t.blk
		}
	}
	for i := 0; i < g.pages && t.err == nil; i++ {
		if i%superPages == 0 {
			closeBlock()
			g.blocks, t.blk = append(g.blocks, block{off: t.off}), 0
		}
		first := t.take(t.u32())
		if i%superPages == 0 {
			g.blocks[len(g.blocks)-1].first = bytes.Clone(first)
		}
		if last := t.take(t.u32()); i == g.pages-1 {
			g.last = bytes.Clone(last)
		}
		t.take(12) // offset and bytes
		sum = sum[:0]
		for k := t.u32(); k > 0 && t.err == nil; k-- {
			sum = append(sum, math.Float64frombits(t.u64()))
		}
		g.blocks[len(g.blocks)-1].sum.add(sum)
	}
	closeBlock()
	g.bloomLen = int(t.u32())
	g.bloomAt = t.off
	for k := 0; k < g.bloomLen && t.err == nil; k += bloomChunk {
		g.chunks = append(g.chunks, crc32.Checksum(t.take(uint64(min(bloomChunk, g.bloomLen-k))), crcTable))
	}
	for n := t.u32(); n > 0 && t.err == nil; n-- {
		if g.Files == nil {
			g.Files = map[uint64]int64{}
		}
		file := t.u64()
		g.Files[file] = int64(t.u64())
	}
	g.Rows, g.MaxSeq = t.u64(), t.u64()
	if t.err != nil || t.off != end || t.all != binary.LittleEndian.Uint32(foot[8:]) || g.bloomLen == 0 {
		return errDamagedIndex
	}
	return nil
}

// memory is what the segment keeps in RAM: per block its first key and its
// sums, per filter chunk a CRC. Index entries and filter chunks in the page
// cache are counted there.
func (g *segment) memory() (n int) {
	for _, b := range g.blocks {
		n += len(b.first) + 8*len(b.sum) + 96
	}
	return n + len(g.last) + 4*len(g.chunks) + 32*len(g.Files)
}

// segWriter builds a segment from rows given in key order. A row that
// carries its payload gets it written to the segment's own payload file; a
// row that only points at a payload keeps pointing.
type segWriter struct {
	bloom     []byte
	files     map[uint64]int64
	nrows     uint64
	maxSeq    uint64
	pages     uint64
	index     []byte // index entries not yet spilled
	spill     File   // NNN.idx.tmp: the index entries past the first MB
	spilled   int64
	fs        FS
	id        uint64
	f, v      File
	bw        *bufio.Writer
	vw        *valWriter
	off, voff uint64
	rows      []*row
	shared    int  // shallowest boundary inside the current page
	wide      bool // a page is wide or f32: the segment ends ZLS4
	width     int  // the widest row of the current page
	held      int  // payload bytes waiting in rows
}

func newSegWriter(fs FS, id, expectRows uint64) (*segWriter, error) {
	w := &segWriter{fs: fs, id: id, shared: locLen * 8}
	w.bloom, w.files = make([]byte, expectRows*bloomBits/8+8), map[uint64]int64{}
	var err error
	if w.f, err = fs.Create(segName(id)); err != nil {
		return nil, err
	}
	w.bw = bufio.NewWriterSize(w.f, 1<<20)
	return w, nil
}

// add appends a row. Rows of one key (a record and its retraction) stay in
// one page, so a seek never lands between them. A page ends at pageRows
// rows or 1 MB of payloads to copy, or earlier, from a quarter full, where the keys cross a zone
// boundary shallower than any inside the page: the children of a large zone
// then start on page boundaries, and their sums need no page at all.
func (w *segWriter) add(r *row) error {
	if n := len(w.rows); n > 0 && !bytes.Equal(w.rows[n-1].key, r.key) {
		shared := sharedDepth(w.rows[n-1].key, r.key)
		// A page is as wide as its widest row: wide rows (embeddings) end it
		// at 2 MB of metric columns rather than at 256 rows.
		wide := (n + 1) * max(w.width, len(r.metrics)) * 8
		if n >= pageRows || w.held >= 1<<20 || wide > 2<<20 || n >= pageRows/4 && shared < w.shared {
			if err := w.flushPage(); err != nil {
				return err
			}
			w.shared, shared = locLen*8, locLen*8
		}
		w.shared = min(w.shared, shared)
	}
	w.rows, w.held = append(w.rows, r), w.held+len(r.payload)
	w.width = max(w.width, len(r.metrics))
	return nil
}

// sharedDepth is how many location characters two keys have in common; below
// zero when they are in different collections.
func sharedDepth(a, b []byte) int {
	for i := range headLen - 1 {
		if x := a[i] ^ b[i]; x != 0 {
			return (8*i+bits.LeadingZeros8(x)-16+6)/6 - 1
		}
	}
	return locLen * 8
}

func (w *segWriter) flushPage() error {
	n, m, idBytes, payloads := len(w.rows), 0, 0, false
	var sum sums
	for _, r := range w.rows {
		m = max(m, len(r.metrics))
		idBytes += len(r.key) - headLen
		payloads = payloads || len(r.payload) > 0 || r.vlen > 0
		sum.addRow(r)
		bloomSet(w.bloom, hash64(r.key))
		w.maxSeq = max(w.maxSeq, r.seq)
	}
	p := layout(n, m, payloads, m >= f32Columns && exactF32(w.rows))
	raw := make([]byte, p.ids, p.ids+idBytes+4)
	if p.f32 {
		raw[11] = 1
		w.wide = true // ZLS4: an older release would misread the column
	}
	binary.LittleEndian.PutUint16(raw, uint16(n))
	if p.wide {
		raw[8] = 1
		binary.LittleEndian.PutUint16(raw[9:], uint16(m))
		w.wide = true
	} else {
		raw[2] = byte(m)
	}
	if payloads {
		raw[3] = 1
	}
	binary.LittleEndian.PutUint32(raw[4:], uint32(idBytes))
	idEnd := 0
	for i, r := range w.rows {
		copy(raw[pageHeader+i*headLen:], r.key[:headLen])
		raw[p.flags+i] = r.flags
		if p.wide {
			binary.LittleEndian.PutUint16(raw[p.nmet+2*i:], uint16(len(r.metrics)))
		} else {
			raw[p.nmet+i] = byte(len(r.metrics))
		}
		idEnd += len(r.key) - headLen
		binary.LittleEndian.PutUint16(raw[p.idEnd+2*i:], uint16(idEnd))
		binary.LittleEndian.PutUint64(raw[p.seq+8*i:], r.seq)
		binary.LittleEndian.PutUint64(raw[p.req+8*i:], r.req)
		binary.LittleEndian.PutUint64(raw[p.weight+8*i:], math.Float64bits(r.weight))
		for c, v := range r.metrics {
			if p.f32 {
				binary.LittleEndian.PutUint32(raw[p.met+4*(n*c+i):], math.Float32bits(float32(v)))
			} else {
				binary.LittleEndian.PutUint64(raw[p.met+8*(n*c+i):], math.Float64bits(v))
			}
		}
		val, voff, vlen := r.val, r.voff, r.vlen
		if len(r.payload) > 0 {
			if err := w.writePayload(r.payload); err != nil {
				return err
			}
			val, voff, vlen = w.id, w.voff, uint32(len(r.payload))
			w.voff += uint64(vlen)
		}
		if vlen > 0 {
			w.files[val] += int64(vlen)
			binary.LittleEndian.PutUint32(raw[p.val+4*i:], uint32(val))
			binary.LittleEndian.PutUint64(raw[p.val+4*n+8*i:], voff)
			binary.LittleEndian.PutUint32(raw[p.val+12*n+4*i:], vlen)
		}
	}
	for _, r := range w.rows {
		raw = append(raw, r.key[headLen:]...)
	}
	raw = binary.LittleEndian.AppendUint32(raw, crc32.Checksum(raw, crcTable))
	if err := w.indexEntry(pageRef{w.rows[0].key, w.rows[n-1].key, w.off, uint32(len(raw)), sum}); err != nil {
		return err
	}
	w.off, w.nrows = w.off+uint64(len(raw)), w.nrows+uint64(n)
	w.rows, w.held, w.width = w.rows[:0], 0, 0
	_, err := w.bw.Write(raw)
	return err
}

// indexSpill is how many bytes of index entries a writer holds before it
// writes them to its spill file.
var indexSpill = 1 << 20

func spillName(id uint64) string { return fmt.Sprintf("%08d.idx.tmp", id) }

// indexEntry encodes the index entry of a page. Past a megabyte the entries
// go to a file of their own, read back by finish: a writer's memory does not
// grow with the segment, apart from its filter.
func (w *segWriter) indexEntry(ref pageRef) error {
	le := binary.LittleEndian
	b := append(le.AppendUint32(w.index, uint32(len(ref.First))), ref.First...)
	b = append(le.AppendUint32(b, uint32(len(ref.Last))), ref.Last...)
	b = le.AppendUint32(le.AppendUint64(b, ref.Off), ref.Size)
	b = le.AppendUint32(b, uint32(len(ref.Sum)))
	for _, x := range ref.Sum {
		b = le.AppendUint64(b, math.Float64bits(x))
	}
	w.index, w.pages = b, w.pages+1
	if len(w.index) < indexSpill {
		return nil
	}
	var err error
	if w.spill == nil {
		if w.spill, err = w.fs.Create(spillName(w.id)); err != nil {
			return err
		}
	}
	if _, err = w.spill.Write(w.index); err == nil {
		w.spilled += int64(len(w.index))
		w.index = w.index[:0]
	}
	return err
}

// abort closes the files of a writer that will not finish.
func (w *segWriter) abort() {
	for _, f := range []File{w.f, w.v, w.spill} {
		if f != nil {
			f.Close()
		}
	}
	if w.spill != nil {
		w.fs.Remove(spillName(w.id))
	}
}

// writeTail writes the tail and the footer, after the pages.
func (w *segWriter) writeTail() error {
	le := binary.LittleEndian
	crc := uint32(0)
	write := func(b []byte) {
		crc = crc32.Update(crc, crcTable, b)
		w.bw.Write(b)
	}
	write(le.AppendUint32(nil, uint32(w.pages)))
	if w.spill != nil {
		buf := make([]byte, 1<<20)
		for at := int64(0); at < w.spilled; {
			n, err := w.spill.ReadAt(buf[:min(int64(len(buf)), w.spilled-at)], at)
			if n == 0 && err != nil {
				return err
			}
			write(buf[:n])
			at += int64(n)
		}
		w.spill.Close()
		w.spill = nil
		if err := w.fs.Remove(spillName(w.id)); err != nil {
			return err
		}
	}
	write(w.index)
	write(le.AppendUint32(nil, uint32(len(w.bloom))))
	write(w.bloom)
	b := le.AppendUint32(nil, uint32(len(w.files)))
	for _, file := range slices.Sorted(maps.Keys(w.files)) {
		b = le.AppendUint64(le.AppendUint64(b, file), uint64(w.files[file]))
	}
	write(le.AppendUint64(le.AppendUint64(b, w.nrows), w.maxSeq))
	w.bw.Write(le.AppendUint32(le.AppendUint64(nil, w.off), crc))
	magic := "ZLS3"
	if w.wide {
		magic = "ZLS4" // an older release refuses it instead of misreading it
	}
	_, err := w.bw.Write([]byte(magic))
	return err
}

func (w *segWriter) writePayload(b []byte) (err error) {
	if w.v == nil {
		if w.v, err = w.fs.Create(valName(w.id)); err != nil {
			return err
		}
		w.vw = newValWriter(w.v)
	}
	_, err = w.vw.Write(b)
	return err
}

// finish writes the tail and the footer, and syncs.
func (w *segWriter) finish(sync bool) error {
	if len(w.rows) > 0 {
		if err := w.flushPage(); err != nil {
			return err
		}
	}
	if err := w.writeTail(); err != nil {
		return err
	}
	for i, f := range []File{w.f, w.v} {
		if f == nil {
			continue
		}
		err := []interface{ Flush() error }{w.bw, w.vw}[i].Flush()
		if err == nil && sync {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
