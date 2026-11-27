package wire

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// IXB2 layout constants; docs/protocol/wire-block.md is the contract.
const (
	SetsBlockContentType = "application/vnd.indexus.block"

	setsBlockHeaderBytes  = 16
	setsBlockSectionBytes = 8
	setsBlockRowsBytes    = 12
	setsBlockKindRows     = 1
	setsBlockKindRedirect = 2
	setsBlockKindColumns  = 3
	// setsBlockKindWideRows is a rows section of more than 255 metrics (an
	// embedding): its header has 4 more bytes and a u32 metric count.
	setsBlockKindWideRows  = 4
	setsBlockWideRowsBytes = 16
	// SetsBlockMaxMetrics is the widest section, the widest record of zonelog.
	SetsBlockMaxMetrics  = 4096
	setsBlockMaxKeyWords = 6
)

var setsBlockMagic = [4]byte{'I', 'X', 'B', '2'}

// SetsBlockMetrics is the width of the metric columns of an IXB2 body.
type SetsBlockMetrics uint8

const (
	// SetsBlockF32 is the default: metrics rounded once to float32.
	SetsBlockF32 SetsBlockMetrics = iota
	// SetsBlockF64 sends the float64 sums as they are (metrics=f64),
	// for collections whose sums must stay exact beyond 24 bits.
	SetsBlockF64
)

const (
	setsBlockFlagRedirects = 1 << 0
	setsBlockFlagF64       = 1 << 1
	setsBlockFlagColumns   = 1 << 2
	setsBlockFlagWide      = 1 << 3
)

// setsBlockIndex maps a routing character to its 6-bit value. A table keeps
// key packing off the linear Base.IndexOf scan while BASE64 stays the only
// alphabet.
var setsBlockIndex = func() (table [256]int8) {
	for i := range table {
		table[i] = -1
	}
	for i := 0; i < 64; i++ {
		table[tree.BASE64.CharAt(i)[0]] = int8(i)
	}
	return table
}()

// setsBlockRow is one item key projected under its requested parent. Metrics
// are copied out under the set lock into a shared arena, so encoding never
// reads an Abelian that a writer may be updating.
type setsBlockRow struct {
	// order packs location, fallback, chars and the first key word so that
	// the sort decides almost every pair with one comparison.
	order    uint64
	location int32
	fallback bool
	chars    uint8
	words    [setsBlockMaxKeyWords]uint32
	count    int
	metrics  int32
	width    int32
}

// setsBlockSection is a run of sorted rows sharing (location, parent, depth).
type setsBlockSection struct {
	parent string
	start  int
	end    int
	unique int
	width  int
	// pad is the zero bytes before the metrics column, which must start on
	// a multiple of its element size from the start of the body.
	pad   int
	bytes int
}

// wide sections (more than 255 metrics) travel as kind 4.
func (s setsBlockSection) wide() bool { return s.width > math.MaxUint8 }

func (s setsBlockSection) rowsBytes() int {
	if s.wide() {
		return setsBlockWideRowsBytes
	}
	return setsBlockRowsBytes
}

// EncodeSetsBlock builds the IXB2 /sets body: count and every metric as
// columns, plus the redirects; width says whether metrics travel as float32
// or float64.
func EncodeSetsBlock(sets map[string]*Set, locations []string, precision int, redirects []SetsRedirect, width SetsBlockMetrics) ([]byte, error) {
	return EncodeSetsBlockColumns(sets, locations, precision, redirects, width, nil)
}

// EncodeSetsBlockColumns is EncodeSetsBlock with only the metrics columns
// names, in that order (nil: every metric). The ids go first, in a columns
// section, so a client knows which metric each column is; counts and sums
// are those of the whole record.
//
// Steps, in order:
//  1. check the request;
//  2. project every item key under its requested location, into rows;
//  3. sort the rows into the order of the contract;
//  4. cut them into sections and size the whole body;
//  5. write the header, the columns section, the rows and the redirects.
func EncodeSetsBlockColumns(sets map[string]*Set, locations []string, precision int, redirects []SetsRedirect, width SetsBlockMetrics, columns []int) ([]byte, error) {
	if err := CheckSetsColumns(columns); err != nil {
		return nil, err
	}
	if precision < 1 || precision > SetsPrecision {
		return nil, fmt.Errorf("sets block: precision %d outside [1, %d]", precision, SetsPrecision)
	}
	if len(locations) > MaxReadLocations {
		return nil, fmt.Errorf("sets block: %d locations exceed %d", len(locations), MaxReadLocations)
	}
	capacity := 0
	for _, location := range locations {
		if set := sets[location]; set != nil {
			capacity += set.size()
		}
	}
	scratch := getSetsBlockScratch(capacity)
	defer putSetsBlockScratch(scratch)
	rows, metrics, err := collectSetsBlockRows(scratch, sets, locations, precision, columns)
	if err != nil {
		return nil, err
	}
	// Keep what append grew for the next body.
	scratch.rows, scratch.metrics = rows[:0], metrics[:0]
	scratch.keys = sortSetsBlockRows(rows, scratch.keys)

	// Size every section first so the body is allocated exactly once.
	size := setsBlockHeaderBytes
	sectionCount := 0
	if columns != nil {
		sectionCount++
		size += setsBlockColumnsBytes(columns)
	}
	sections, size, err := sizeSetsBlockSections(rows, locations, width, size)
	if err != nil {
		return nil, err
	}
	sectionCount += len(sections)
	if len(redirects) > 0 {
		sectionCount++
		size += setsBlockRedirectsBytes(redirects)
	}

	out := make([]byte, setsBlockHeaderBytes, size)
	copy(out, setsBlockMagic[:])
	binary.LittleEndian.PutUint16(out[4:], setsBlockHeaderBytes)
	binary.LittleEndian.PutUint16(out[6:], setsBlockFlags(sections, redirects, width, columns))
	binary.LittleEndian.PutUint32(out[8:], uint32(sectionCount))
	if columns != nil {
		out = appendSetsBlockColumns(out, columns)
	}
	for _, section := range sections {
		out = appendSetsBlockRows(out, section, rows, metrics, width)
	}
	if len(redirects) > 0 {
		out = appendSetsBlockRedirects(out, redirects)
	}
	return out, nil
}

// setsBlockCollector turns the items of the requested locations into rows,
// one location at a time: add is the callback of Set.Traverse. It keeps the
// first failure and ignores the items that follow it.
type setsBlockCollector struct {
	rows      []setsBlockRow
	metrics   []float64
	precision int
	columns   []int
	// index and parent are those of the location being traversed.
	index   int
	parent  string
	failure error
}

// collectSetsBlockRows projects every item of the requested locations into
// the scratch arenas and returns them as append grew them. A location without
// a set is skipped.
func collectSetsBlockRows(scratch *setsBlockScratch, sets map[string]*Set, locations []string, precision int, columns []int) ([]setsBlockRow, []float64, error) {
	c := setsBlockCollector{rows: scratch.rows, metrics: scratch.metrics, precision: precision, columns: columns}
	for index, location := range locations {
		set, exist := sets[location]
		if !exist || set == nil {
			continue
		}
		c.index, c.parent = index, location
		if c.parent == tree.BASE64.Root() {
			c.parent = ""
		}
		if len(c.parent) > math.MaxUint8 {
			return nil, nil, fmt.Errorf("sets block: parent %q too long", c.parent)
		}
		set.Traverse(c.add)
		if c.failure != nil {
			return nil, nil, c.failure
		}
	}
	return c.rows, c.metrics, nil
}

// add appends the row of one item: its key cut at the aggregation suffix and
// at the precision, then packed as the suffix under the parent, or whole and
// flagged fallback when the parent is not its prefix.
func (c *setsBlockCollector) add(key string, value *Abelian) {
	if idx := strings.IndexByte(key, ':'); idx >= 0 {
		key = key[:idx]
	}
	if len(key) > c.precision {
		key = key[:c.precision]
	}
	if key == "" || c.failure != nil {
		return
	}
	row := setsBlockRow{location: int32(c.index), count: value.Count()}
	suffix := key
	if strings.HasPrefix(key, c.parent) {
		suffix = key[len(c.parent):]
	} else {
		row.fallback = true
	}
	row.chars = uint8(len(suffix))
	if !packSetsBlockKey(&row.words, suffix) {
		c.failure = fmt.Errorf("sets block: key %q outside the routing alphabet", key)
		return
	}
	row.order = uint64(c.index)<<39 | uint64(row.chars)<<32 | uint64(row.words[0])
	if row.fallback {
		row.order |= 1 << 38
	}
	row.metrics = int32(len(c.metrics))
	if c.columns != nil {
		row.width = int32(len(c.columns))
		for _, column := range c.columns {
			c.metrics = append(c.metrics, value.Metric(column))
		}
	} else {
		row.width = int32(len(value.metrics))
		for j := range value.metrics {
			c.metrics = append(c.metrics, value.Metric(j))
		}
	}
	c.rows = append(c.rows, row)
}

// sizeSetsBlockSections cuts the sorted rows into sections and lays them out
// one after the other from offset size. It returns the sections and the
// offset after the last one.
func sizeSetsBlockSections(rows []setsBlockRow, locations []string, width SetsBlockMetrics, size int) ([]setsBlockSection, int, error) {
	var sections []setsBlockSection
	for start := 0; start < len(rows); {
		section := cutSetsBlockSection(rows, start, locations)
		if section.width > SetsBlockMaxMetrics {
			return nil, 0, fmt.Errorf("sets block: %d metrics exceed one section", section.width)
		}
		size = section.place(size, int(rows[start].chars), width)
		sections = append(sections, section)
		start = section.end
	}
	return sections, size, nil
}

// cutSetsBlockSection is the section starting at rows[start]: the run of rows
// of the same (location, parent, depth), with its count of distinct keys and
// its widest row. Its layout is not set yet (place).
func cutSetsBlockSection(rows []setsBlockRow, start int, locations []string) setsBlockSection {
	head := rows[start]
	section := setsBlockSection{start: start}
	if !head.fallback {
		section.parent = locations[head.location]
		if section.parent == tree.BASE64.Root() {
			section.parent = ""
		}
	}
	end := start
	for end < len(rows) && sameSetsBlockSection(head, rows[end]) {
		if end == start || rows[end].words != rows[end-1].words {
			section.unique++
		}
		section.width = max(section.width, int(rows[end].width))
		end++
	}
	section.end = end
	return section
}

// place lays the section out at offset in the body, for keys of chars
// characters: it sets the padding before float64 metrics and the section's
// size, and returns the offset after the section.
func (s *setsBlockSection) place(offset, chars int, width SetsBlockMetrics) int {
	words := setsBlockKeyWords(chars)
	size := offset + setsBlockSectionBytes + s.rowsBytes() + pad4(len(s.parent))
	size += 4 * s.unique * (words + 1)
	if width == SetsBlockF64 && s.width > 0 {
		s.pad = pad8(size) - size
		size += s.pad + 8*s.unique*s.width
	} else {
		size += 4 * s.unique * s.width
	}
	s.bytes = size - offset
	return size
}

func setsBlockFlags(sections []setsBlockSection, redirects []SetsRedirect, width SetsBlockMetrics, columns []int) uint16 {
	var flags uint16
	if len(redirects) > 0 {
		flags |= setsBlockFlagRedirects
	}
	if width == SetsBlockF64 {
		flags |= setsBlockFlagF64
	}
	if columns != nil {
		flags |= setsBlockFlagColumns
	}
	for _, section := range sections {
		if section.wide() {
			flags |= setsBlockFlagWide
		}
	}
	return flags
}

// setsBlockColumnsBytes is the size of the columns section.
func setsBlockColumnsBytes(columns []int) int {
	return setsBlockSectionBytes + 4 + pad4(2*len(columns))
}

func appendSetsBlockColumns(out []byte, columns []int) []byte {
	out = binary.LittleEndian.AppendUint32(out, uint32(setsBlockColumnsBytes(columns)))
	out = binary.LittleEndian.AppendUint16(out, setsBlockKindColumns)
	out = binary.LittleEndian.AppendUint16(out, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(columns)))
	for _, c := range columns {
		out = binary.LittleEndian.AppendUint16(out, uint16(c))
	}
	for len(out)%4 != 0 {
		out = append(out, 0)
	}
	return out
}

// setsBlockRedirectsBytes is the size of the redirects section.
func setsBlockRedirectsBytes(redirects []SetsRedirect) int {
	bytes := setsBlockSectionBytes + 4
	for _, r := range redirects {
		bytes += pad4(8 + len(r.Location) + len(r.Name) + len(r.IP))
	}
	return bytes
}

func appendSetsBlockRows(out []byte, section setsBlockSection, rows []setsBlockRow, metrics []float64, width SetsBlockMetrics) []byte {
	chars := int(rows[section.start].chars)
	words := setsBlockKeyWords(chars)
	n := section.unique
	out = binary.LittleEndian.AppendUint32(out, uint32(section.bytes))
	if section.wide() {
		out = binary.LittleEndian.AppendUint16(out, setsBlockKindWideRows)
		out = binary.LittleEndian.AppendUint16(out, 0)
		out = binary.LittleEndian.AppendUint32(out, uint32(n))
		out = append(out, byte(len(section.parent)), byte(chars), byte(words), 0)
		out = binary.LittleEndian.AppendUint32(out, 0)
		out = binary.LittleEndian.AppendUint32(out, uint32(section.width))
	} else {
		out = binary.LittleEndian.AppendUint16(out, setsBlockKindRows)
		out = binary.LittleEndian.AppendUint16(out, 0)
		out = binary.LittleEndian.AppendUint32(out, uint32(n))
		out = append(out, byte(len(section.parent)), byte(chars), byte(words), byte(section.width))
		out = binary.LittleEndian.AppendUint32(out, 0)
	}
	out = appendPadded(out, section.parent)

	// Each run of equal keys becomes one row: counts and metrics are summed in
	// float64 and rounded to float32 once, as for a key that never collided,
	// or written as they are in float64. Padding bytes stay as make zeroed them.
	keys := len(out)
	counts := keys + 4*n*words
	columns := counts + 4*n + section.pad
	out = out[:columns+metricBytes(width)*n*section.width]
	row := 0
	for i := section.start; i < section.end; row++ {
		run := i + 1
		for run < section.end && rows[run].words == rows[i].words {
			run++
		}
		for w := 0; w < words; w++ {
			binary.LittleEndian.PutUint32(out[keys+4*(row*words+w):], rows[i].words[w])
		}
		count := 0
		for k := i; k < run; k++ {
			count += rows[k].count
		}
		binary.LittleEndian.PutUint32(out[counts+4*row:], uint32(count))
		for j := 0; j < section.width; j++ {
			sum := 0.0
			for k := i; k < run; k++ {
				if j < int(rows[k].width) {
					sum += metrics[int(rows[k].metrics)+j]
				}
			}
			if width == SetsBlockF64 {
				binary.LittleEndian.PutUint64(out[columns+8*(j*n+row):], math.Float64bits(sum))
			} else {
				binary.LittleEndian.PutUint32(out[columns+4*(j*n+row):], math.Float32bits(float32(sum)))
			}
		}
		i = run
	}
	return out
}

func appendSetsBlockRedirects(out []byte, redirects []SetsRedirect) []byte {
	out = binary.LittleEndian.AppendUint32(out, uint32(setsBlockRedirectsBytes(redirects)))
	out = binary.LittleEndian.AppendUint16(out, setsBlockKindRedirect)
	out = binary.LittleEndian.AppendUint16(out, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(redirects)))
	for _, r := range redirects {
		start := len(out)
		out = binary.LittleEndian.AppendUint16(out, uint16(len(r.Location)))
		out = binary.LittleEndian.AppendUint16(out, uint16(len(r.Name)))
		out = binary.LittleEndian.AppendUint16(out, uint16(len(r.IP)))
		out = binary.LittleEndian.AppendUint16(out, uint16(r.Port))
		out = append(out, r.Location...)
		out = append(out, r.Name...)
		out = append(out, r.IP...)
		for (len(out)-start)%4 != 0 {
			out = append(out, 0)
		}
	}
	return out
}

// packSetsBlockKey writes the suffix as 6-bit routing indices, MSB-first.
func packSetsBlockKey(words *[setsBlockMaxKeyWords]uint32, suffix string) bool {
	var buffer uint64
	var bits uint
	w := 0
	for i := 0; i < len(suffix); i++ {
		value := setsBlockIndex[suffix[i]]
		if value < 0 {
			return false
		}
		buffer = buffer<<6 | uint64(value)
		bits += 6
		if bits >= 32 {
			bits -= 32
			words[w] = uint32(buffer >> bits)
			buffer &= 1<<bits - 1
			w++
		}
	}
	if bits > 0 {
		words[w] = uint32(buffer << (32 - bits))
	}
	return true
}

// setsBlockSortKey is a row's order and its position before sorting. Moving
// 16 bytes per pass instead of the whole row is what makes radix pay off.
type setsBlockSortKey struct {
	order uint64
	index uint32
}

// sortSetsBlockRows sorts rows by (order, words[1:]), the order of the
// contract. order holds the section and the first key word, so an LSD radix
// on it decides every row with k = 1; the bytes that are equal for all rows
// (high location bits, unused chars) cost no pass. Ties left by keys of two
// words or more are rare and short, and are settled by comparison.
func sortSetsBlockRows(rows []setsBlockRow, keys []setsBlockSortKey) []setsBlockSortKey {
	n := len(rows)
	if n < 2 {
		return keys
	}
	if cap(keys) < 2*n {
		keys = make([]setsBlockSortKey, 2*n)
	}
	keys = keys[:2*n]
	from, to := keys[:n], keys[n:]
	var histogram [8][256]uint32
	for i := range rows {
		order := rows[i].order
		from[i] = setsBlockSortKey{order: order, index: uint32(i)}
		for digit := range 8 {
			histogram[digit][byte(order>>(8*digit))]++
		}
	}
	for digit := range 8 {
		counts := &histogram[digit]
		if counts[byte(from[0].order>>(8*digit))] == uint32(n) {
			continue
		}
		var offsets [256]uint32
		sum := uint32(0)
		for b, c := range counts {
			offsets[b] = sum
			sum += c
		}
		for _, key := range from {
			b := byte(key.order >> (8 * digit))
			to[offsets[b]] = key
			offsets[b]++
		}
		from, to = to, from
	}
	for start := 0; start < n; {
		end := start + 1
		for end < n && from[end].order == from[start].order {
			end++
		}
		if end-start > 1 && rows[from[start].index].chars > 32/6 {
			slices.SortFunc(from[start:end], func(a, b setsBlockSortKey) int {
				return slices.Compare(rows[a.index].words[1:], rows[b.index].words[1:])
			})
		}
		start = end
	}
	// Apply the permutation in place, one cycle at a time: from[i].index is
	// the row that belongs at i, and a visited slot is marked with n.
	done := uint32(n)
	for i := range from {
		if from[i].index == done || from[i].index == uint32(i) {
			continue
		}
		row := rows[i]
		j := i
		for {
			source := from[j].index
			from[j].index = done
			if source == uint32(i) {
				rows[j] = row
				break
			}
			rows[j] = rows[source]
			j = int(source)
		}
	}
	return keys
}

// setsBlockScratch is the working memory of one encode — rows, the metrics
// arena and the sort keys, about five times the body — kept between bodies
// so a read allocates its output only.
type setsBlockScratch struct {
	rows    []setsBlockRow
	metrics []float64
	keys    []setsBlockSortKey
}

// setsBlockScratchMaxPooled bounds what an idle scratch may keep, so one huge
// read does not pin its memory for the life of the process.
const setsBlockScratchMaxPooled = 32 << 20

var setsBlockScratches sync.Pool

func getSetsBlockScratch(capacity int) *setsBlockScratch {
	s, _ := setsBlockScratches.Get().(*setsBlockScratch)
	if s == nil {
		s = &setsBlockScratch{}
	}
	if cap(s.rows) < capacity {
		s.rows = make([]setsBlockRow, 0, capacity)
	}
	if cap(s.metrics) < capacity*SetsPropertyCount {
		s.metrics = make([]float64, 0, capacity*SetsPropertyCount)
	}
	return s
}

func putSetsBlockScratch(s *setsBlockScratch) {
	bytes := cap(s.rows)*int(unsafe.Sizeof(setsBlockRow{})) + 8*cap(s.metrics) + int(unsafe.Sizeof(setsBlockSortKey{}))*cap(s.keys)
	if bytes > setsBlockScratchMaxPooled {
		return
	}
	s.rows, s.metrics, s.keys = s.rows[:0], s.metrics[:0], s.keys[:0]
	setsBlockScratches.Put(s)
}

func sameSetsBlockSection(a, b setsBlockRow) bool {
	return a.order>>32 == b.order>>32
}

func setsBlockKeyWords(chars int) int {
	return (6*chars + 31) / 32
}

func pad4(n int) int {
	return (n + 3) &^ 3
}

func pad8(n int) int {
	return (n + 7) &^ 7
}

func metricBytes(width SetsBlockMetrics) int {
	if width == SetsBlockF64 {
		return 8
	}
	return 4
}

func appendPadded(out []byte, s string) []byte {
	out = append(out, s...)
	for i := len(s); i%4 != 0; i++ {
		out = append(out, 0)
	}
	return out
}

// CheckSetsColumns accepts nil (every metric) or 1 to SetsBlockMaxMetrics
// distinct metric positions.
func CheckSetsColumns(columns []int) error {
	if columns == nil {
		return nil
	}
	if len(columns) == 0 || len(columns) > SetsBlockMaxMetrics {
		return fmt.Errorf("sets block: %d columns, want 1 to %d", len(columns), SetsBlockMaxMetrics)
	}
	seen := map[int]bool{}
	for _, c := range columns {
		if c < 0 || c >= math.MaxUint16 || seen[c] {
			return fmt.Errorf("sets block: column %d out of range or repeated", c)
		}
		seen[c] = true
	}
	return nil
}
