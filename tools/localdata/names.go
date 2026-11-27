package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// splitBits bounds the search: spatial records differ early in their
// location, and content records (hashed paths) are uniform on every bit.
const splitBits = 48

// threeNames returns three vnode names whose XOR cells share the records
// read from input as evenly as the bits allow. The three names are equal but
// at bits i < j: name 0 differs from the other two at i, names 1 and 2 differ
// at j. The owner of a zone then depends only on the zone's bits i and j, so
// every (i, j) choice can be scored from per-pair tables. Native records
// (counted) and content parts (weighed in bytes) are scored apart, each by
// the sum of the squared shares of the three vnodes: it is lowest when every
// vnode holds a part of both, which a single largest share does not see.
func threeNames(input io.Reader) ([3]string, error) {
	var native, content [splitBits][splitBits][4]float64
	var records, bytes float64
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 65536), 4<<20)
	for scan.Scan() {
		var r storage.Record
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			return [3]string{}, err
		}
		id, err := tree.ZoneID(r.Collection, r.Location)
		if err != nil {
			return [3]string{}, err
		}
		table, weight := &native, 1.0
		if _, part := contentPath(r); part {
			table, weight = &content, float64(len(scan.Bytes()))
			bytes += weight
		} else {
			records++
		}
		for i := range splitBits {
			bi := tree.Bit(id, uint8(i)) << 1
			for j := i + 1; j < splitBits; j++ {
				table[i][j][bi|tree.Bit(id, uint8(j))] += weight
			}
		}
	}
	if err := scan.Err(); err != nil {
		return [3]string{}, err
	}
	if records+bytes == 0 {
		return [3]string{}, fmt.Errorf("names: no records")
	}
	best, bi, bj, ba, bb := 3.0, 0, 1, uint8(0), uint8(0)
	for i := range splitBits {
		for j := i + 1; j < splitBits; j++ {
			for a := range uint8(2) {
				for b := range uint8(2) {
					// Cells are indexed by bit i (high) and bit j (low).
					squares := func(table [4]float64, total float64) float64 {
						if total == 0 {
							return 0
						}
						sum := 0.0
						for _, held := range []float64{table[a<<1] + table[a<<1|1], table[(1-a)<<1|b], table[(1-a)<<1|(1-b)]} {
							sum += (held / total) * (held / total)
						}
						return sum
					}
					score := squares(native[i][j], records) + squares(content[i][j], bytes)
					if score < best {
						best, bi, bj, ba, bb = score, i, j, a, b
					}
				}
			}
		}
	}
	// The last bit keeps every name away from the reserved zero ID; it is
	// shared by the three and so never decides an owner.
	var base tree.ID
	base = tree.SetBit(base, tree.Size*8-1, 1)
	first := tree.SetBit(base, uint8(bi), ba)
	second := tree.SetBit(tree.SetBit(base, uint8(bi), 1-ba), uint8(bj), bb)
	third := tree.SetBit(tree.SetBit(base, uint8(bi), 1-ba), uint8(bj), 1-bb)
	return [3]string{tree.EncodeName(first), tree.EncodeName(second), tree.EncodeName(third)}, nil
}
