// Package zonelog is a prototype store for Indexus records: one append-only
// log of mutations, sorted immutable segments, and zone sums read from the
// segments. Standard library only. See docs/storage/zonelog.md.
package zonelog

import (
	"encoding/binary"
	"fmt"
)

// The routing alphabet: a character is 6 bits, its index here.
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

// A key is collection number (2 bytes), location bits (12 bytes, left
// aligned), depth in characters (1 byte), then the record id. Keys sorted as
// bytes keep everything below a zone in one contiguous range, and a zone and
// its first child, which share their location bits, apart by the depth byte.
const (
	locLen  = 12
	headLen = 2 + locLen + 1
	maxID   = 255
)

var charIndex = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	for i := range len(alphabet) {
		t[alphabet[i]] = int8(i)
	}
	return t
}()

// locBits packs a location into 96 bits. The root is "@" or "".
func locBits(location string) (bits [locLen]byte, depth int, err error) {
	if location == "@" || location == "" {
		return bits, 0, nil
	}
	if len(location) > locLen*8/6 {
		return bits, 0, fmt.Errorf("zonelog: location %q is too long", location)
	}
	for i := range len(location) {
		v := charIndex[location[i]]
		if v < 0 {
			return bits, 0, fmt.Errorf("zonelog: location %q: bad character", location)
		}
		// Character i owns bits [6i, 6i+6), most significant first.
		at := 6 * i
		w := uint16(v) << 10 >> (at % 8)
		bits[at/8] |= byte(w >> 8)
		if at/8+1 < locLen {
			bits[at/8+1] |= byte(w)
		}
	}
	return bits, len(location), nil
}

func locString(bits []byte, depth int) string {
	if depth == 0 {
		return "@"
	}
	out := make([]byte, depth)
	for i := range depth {
		at := 6 * i
		w := uint16(bits[at/8]) << 8
		if at/8+1 < locLen {
			w |= uint16(bits[at/8+1])
		}
		out[i] = alphabet[w<<(at%8)>>10]
	}
	return string(out)
}

func makeKey(col uint16, location, id string) ([]byte, error) {
	bits, depth, err := locBits(location)
	if err != nil {
		return nil, err
	}
	if len(id) > maxID {
		return nil, fmt.Errorf("zonelog: id longer than %d bytes", maxID)
	}
	key := make([]byte, headLen, headLen+len(id))
	binary.BigEndian.PutUint16(key, col)
	copy(key[2:], bits[:])
	key[headLen-1] = byte(depth)
	return append(key, id...), nil
}

// zoneRange returns the keys [lo, hi) that hold every record at or below a
// zone. hi is the first key after the zone: the next prefix at depth 0, or
// the next collection after the last zone.
func zoneRange(col uint16, location string) (lo, hi []byte, err error) {
	if lo, err = makeKey(col, location, ""); err != nil {
		return nil, nil, err
	}
	depth := int(lo[headLen-1])
	hi = make([]byte, headLen)
	copy(hi, lo)
	hi[headLen-1] = 0
	// Add one to the last bit of the prefix, carrying up into the collection.
	for bit := 16 + 6*depth - 1; bit >= 0; bit-- {
		mask := byte(0x80) >> (bit & 7)
		hi[bit>>3] ^= mask
		if hi[bit>>3]&mask != 0 {
			break
		}
	}
	return lo, hi, nil
}

// CompareLocations orders two locations as their keys sort: by the routing
// alphabet's index, character by character, a missing character counting as
// index 0 (the bits a key pads with), then the shallower first. This is not
// Go's string order: '-' and '_' come last, digits first.
func CompareLocations(a, b string) int {
	if a == "@" {
		a = ""
	}
	if b == "@" {
		b = ""
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int8
		if i < len(a) {
			x = charIndex[a[i]]
		}
		if i < len(b) {
			y = charIndex[b[i]]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}
