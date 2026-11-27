package wire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// SetsBlock is a decoded IXB2 body (docs/protocol/wire-block.md). The JSON
// names are those of the golden expectations written by reference.py.
type SetsBlock struct {
	Sections  []SetsBlockSection `json:"sections"`
	Redirects []SetsRedirect     `json:"redirects"`
	// Columns are the metric positions the columns of every section hold,
	// when the read selected some (columns=...); nil: metric j is column j.
	Columns []int `json:"columns,omitempty"`
}

// SetsBlockSection is one rows section: the keys below Parent that share a
// suffix length, their counts and their metric columns.
type SetsBlockSection struct {
	Parent      string      `json:"parent"`
	SuffixChars int         `json:"suffixChars"`
	KeyWords    int         `json:"keyWords"`
	MetricCount int         `json:"metricCount"`
	Revision    uint32      `json:"revision"`
	MetricBytes int         `json:"metricBytes"`
	Suffixes    []string    `json:"suffixes"`
	Keys        []uint32    `json:"keys"`
	Count       []uint32    `json:"count"`
	Metrics     [][]float64 `json:"metrics"`
}

// DecodeSetsBlock reads an IXB2 body: a node uses it on what a peer answers
// to a relayed read. It rejects what the contract rejects, and a rows section
// that its columns do not fill exactly.
func DecodeSetsBlock(buf []byte) (SetsBlock, error) {
	var out SetsBlock
	if len(buf) < setsBlockHeaderBytes || !bytes.Equal(buf[:4], setsBlockMagic[:]) {
		return out, fmt.Errorf("sets block: bad magic")
	}
	if binary.LittleEndian.Uint16(buf[4:]) != setsBlockHeaderBytes {
		return out, fmt.Errorf("sets block: bad header size")
	}
	f64 := binary.LittleEndian.Uint16(buf[6:])&setsBlockFlagF64 != 0
	sectionCount := int(binary.LittleEndian.Uint32(buf[8:]))
	offset := setsBlockHeaderBytes
	for s := 0; s < sectionCount; s++ {
		if len(buf)-offset < setsBlockSectionBytes {
			return out, fmt.Errorf("sets block: truncated section header")
		}
		size := int(binary.LittleEndian.Uint32(buf[offset:]))
		kind := binary.LittleEndian.Uint16(buf[offset+4:])
		if size < setsBlockSectionBytes || size%4 != 0 || size > len(buf)-offset {
			return out, fmt.Errorf("sets block: section overruns body")
		}
		start := offset + setsBlockSectionBytes
		body := buf[start : offset+size]
		offset += size
		var err error
		switch kind {
		case setsBlockKindRows:
			var section SetsBlockSection
			section, err = decodeSetsBlockRows(body, start, f64, false)
			out.Sections = append(out.Sections, section)
		case setsBlockKindWideRows:
			var section SetsBlockSection
			section, err = decodeSetsBlockRows(body, start, f64, true)
			out.Sections = append(out.Sections, section)
		case setsBlockKindRedirect:
			out.Redirects, err = decodeSetsBlockRedirects(body)
		case setsBlockKindColumns:
			out.Columns, err = decodeSetsBlockColumns(body)
		}
		if err != nil {
			return out, err
		}
	}
	if offset != len(buf) {
		return out, fmt.Errorf("sets block: trailing bytes after sections")
	}
	return out, nil
}

// Entries folds the rows into the key → Abelian map a Set is built from:
// each key is its section's parent followed by its suffix, and a key named by
// two sections is summed, as DecodeSetsBinary does for v1.
func (b SetsBlock) Entries() map[string]*Abelian {
	out := make(map[string]*Abelian)
	for _, section := range b.Sections {
		for i, suffix := range section.Suffixes {
			metrics := make([]float64, section.MetricCount)
			for j := range metrics {
				metrics[j] = section.Metrics[j][i]
			}
			if b.Columns != nil { // back to the record's positions
				wide := make([]float64, slices.Max(b.Columns)+1)
				for j, c := range b.Columns {
					if j < len(metrics) {
						wide[c] = metrics[j]
					}
				}
				metrics = wide
			}
			next := NewAbelian(int(section.Count[i]), metrics)
			key := section.Parent + suffix
			if existing, ok := out[key]; ok {
				existing.Sum(next)
			} else {
				out[key] = next
			}
		}
	}
	return out
}

// start is where body begins in the whole block: f64 metrics are aligned on
// 8 bytes from there.
// wide is kind 4: a 16-byte header whose u32 metric count is over 255.
func decodeSetsBlockRows(body []byte, start int, f64, wide bool) (SetsBlockSection, error) {
	var section SetsBlockSection
	section.MetricBytes = 4
	if f64 {
		section.MetricBytes = 8
	}
	header := setsBlockRowsBytes
	if wide {
		header = setsBlockWideRowsBytes
	}
	if len(body) < header {
		return section, fmt.Errorf("sets block: truncated rows header")
	}
	n := int(binary.LittleEndian.Uint32(body))
	parentLength := int(body[4])
	section.SuffixChars = int(body[5])
	section.KeyWords = int(body[6])
	section.MetricCount = int(body[7])
	section.Revision = binary.LittleEndian.Uint32(body[8:])
	if wide {
		if body[7] != 0 {
			return section, fmt.Errorf("sets block: wide rows with a narrow metric count")
		}
		section.MetricCount = int(binary.LittleEndian.Uint32(body[12:]))
		if section.MetricCount <= math.MaxUint8 || section.MetricCount > SetsBlockMaxMetrics {
			return section, fmt.Errorf("sets block: %d metrics in a wide rows section", section.MetricCount)
		}
	}
	if section.KeyWords != setsBlockKeyWords(section.SuffixChars) {
		return section, fmt.Errorf("sets block: keyWords %d for %d chars", section.KeyWords, section.SuffixChars)
	}
	at := header + pad4(parentLength)
	metricsAt := at + 4*n*(section.KeyWords+1)
	if f64 && section.MetricCount > 0 {
		metricsAt = pad8(start+metricsAt) - start
	}
	if n < 1 || len(body) != metricsAt+section.MetricBytes*n*section.MetricCount {
		return section, fmt.Errorf("sets block: rows do not fill the section")
	}
	section.Parent = string(body[header : header+parentLength])
	read := func(count int) []uint32 {
		values := make([]uint32, count)
		for i := range values {
			values[i] = binary.LittleEndian.Uint32(body[at:])
			at += 4
		}
		return values
	}
	section.Keys = read(n * section.KeyWords)
	section.Count = read(n)
	for ; at < metricsAt; at++ {
		if body[at] != 0 {
			return section, fmt.Errorf("sets block: non-zero padding")
		}
	}
	section.Metrics = [][]float64{}
	for j := 0; j < section.MetricCount; j++ {
		column := make([]float64, n)
		for i := range column {
			if f64 {
				column[i] = math.Float64frombits(binary.LittleEndian.Uint64(body[at:]))
			} else {
				column[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(body[at:])))
			}
			at += section.MetricBytes
		}
		section.Metrics = append(section.Metrics, column)
	}
	section.Suffixes = make([]string, n)
	for i := range section.Suffixes {
		section.Suffixes[i] = unpackSetsBlockKey(section.Keys[i*section.KeyWords:(i+1)*section.KeyWords], section.SuffixChars)
	}
	return section, nil
}

func unpackSetsBlockKey(words []uint32, chars int) string {
	var b strings.Builder
	for i := 0; i < chars; i++ {
		bit := 6 * i
		value := 0
		for k := 0; k < 6; k++ {
			position := bit + k
			value = value<<1 | int(words[position/32]>>(31-position%32)&1)
		}
		b.WriteString(tree.BASE64.CharAt(value))
	}
	return b.String()
}

func decodeSetsBlockRedirects(body []byte) ([]SetsRedirect, error) {
	if len(body) < 4 {
		return nil, fmt.Errorf("sets block: truncated redirects")
	}
	count := int(binary.LittleEndian.Uint32(body))
	if count > (len(body)-4)/8 {
		return nil, fmt.Errorf("sets block: %d redirects do not fit the section", count)
	}
	at := 4
	out := make([]SetsRedirect, 0, count)
	for i := 0; i < count; i++ {
		if len(body)-at < 8 {
			return nil, fmt.Errorf("sets block: truncated redirect")
		}
		lengths := [3]int{}
		for k := range lengths {
			lengths[k] = int(binary.LittleEndian.Uint16(body[at+2*k:]))
		}
		port := int(binary.LittleEndian.Uint16(body[at+6:]))
		total := pad4(8 + lengths[0] + lengths[1] + lengths[2])
		if total > len(body)-at {
			return nil, fmt.Errorf("sets block: truncated redirect strings")
		}
		text := body[at+8:]
		out = append(out, SetsRedirect{
			Location: string(text[:lengths[0]]),
			Name:     string(text[lengths[0] : lengths[0]+lengths[1]]),
			IP:       string(text[lengths[0]+lengths[1] : lengths[0]+lengths[1]+lengths[2]]),
			Port:     port,
		})
		at += total
	}
	return out, nil
}

func decodeSetsBlockColumns(body []byte) ([]int, error) {
	if len(body) < 4 {
		return nil, fmt.Errorf("sets block: truncated columns")
	}
	n := int(binary.LittleEndian.Uint32(body))
	if n == 0 || len(body) != 4+pad4(2*n) {
		return nil, fmt.Errorf("sets block: columns section of %d bytes for %d columns", len(body), n)
	}
	out := make([]int, n)
	for i := range out {
		out[i] = int(binary.LittleEndian.Uint16(body[4+2*i:]))
	}
	return out, CheckSetsColumns(out)
}
