package algebra

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// The Space of a collection: how a point of R^k becomes an address, and back.
//
// Mathematically, an address is the interleaving of per-axis dyadic
// quantisations, followed by a per-character rotation:
//
//	axis a, root interval [min_a, max_a], cut n_a(d) times in the first d characters
//	j_a(x) = clamp(ceil(2^n (x_a - min_a) / (max_a - min_a)) - 1, 0, 2^n - 1)   the dyadic cell of x_a
//	bit k of the address = the next bit (most significant first) of j_{σ(k)}   σ is the Schedule
//	character l = A[(bits of l + rot(l)) mod 2^c]                             rot(l) = index of name[l mod 16]
//
// A cell is therefore the box Π [min_a + j_a Δ_a / 2^n_a, min_a + (j_a+1) Δ_a / 2^n_a],
// left-open: (lo, hi], except the first cell of each axis which includes min_a.
//
// Implementations compute it by bisection in binary64 (the SDK rule): at bit
// k, mid = (lo + hi) / 2 on axis σ(k), bit 1 and lo = mid when x > mid, else
// bit 0 and hi = mid. Bisection equals the exact quantisation while every
// cell boundary is a binary64 value, that is for n_a <= Dyadic(a) cuts
// (DyadicLevel). Descriptions must stay within it.
type Space struct {
	Axes []Axis
	// Schedule[k] is the axis cut by bit k of the address, for every bit
	// (BitsPerChar * Width entries). This is the mask, exhaustive: any
	// assignment is allowed, as long as every axis is cut at least once.
	Schedule []uint8
}

// Axis is one real coordinate with its root interval. Nothing else: how a
// client reads or draws the axis (units, wrap-around, projections) is not
// part of the description.
type Axis struct {
	Min, Max float64
}

// RoundRobin is today's only schedule: axes 0, 1, ..., k-1, repeated.
func RoundRobin(axes, bits int) []uint8 {
	s := make([]uint8, bits)
	for k := range s {
		s[k] = uint8(k % axes)
	}
	return s
}

// v2 is the 2-adic valuation of a dyadic rational (binary64 values are dyadic).
func v2(r *big.Rat) int {
	if r.Sign() == 0 {
		return math.MaxInt32
	}
	return int(new(big.Int).Abs(r.Num()).TrailingZeroBits()) - int(r.Denom().TrailingZeroBits())
}

// DyadicLevel is the number of cuts of the axis for which every cell
// boundary min + j (max - min) / 2^n is exactly a binary64 value: up to it,
// bisection is the exact dyadic quantisation, on every platform.
func (a Axis) DyadicLevel() int {
	lo, hi := new(big.Rat).SetFloat64(a.Min), new(big.Rat).SetFloat64(a.Max)
	delta := new(big.Rat).Sub(hi, lo)
	m := new(big.Rat).Abs(lo)
	if h := new(big.Rat).Abs(hi); h.Cmp(m) > 0 {
		m = h
	}
	vMin, vDelta := v2(lo), v2(delta)
	n := 0
	for ; n < 1100; n++ {
		e := vMin
		if d := vDelta - (n + 1); d < e {
			e = d
		}
		// Every boundary of level n+1 is a multiple of 2^e not above m: representable iff m <= 2^53 * 2^e.
		lim := new(big.Rat).SetFloat64(math.Ldexp(1, 53+e))
		if math.IsInf(math.Ldexp(1, 53+e), 0) || m.Cmp(lim) > 0 {
			break
		}
	}
	return n
}

// BitsAt counts the cuts of each axis in the first depth characters.
func (s Space) BitsAt(bitsPerChar, depth int) []int {
	out := make([]int, len(s.Axes))
	for k := 0; k < depth*bitsPerChar && k < len(s.Schedule); k++ {
		out[s.Schedule[k]]++
	}
	return out
}

// ValidateSpace checks the space against the address geometry and the
// deepest depth records use.
func (s Space) ValidateSpace(bitsPerChar, width, recordDepth int) error {
	if len(s.Axes) > 255 {
		return fmt.Errorf("%w: a space has at most 255 axes", ErrSchema)
	}
	if len(s.Axes) == 0 {
		// Opaque addresses: the writer chooses every character (a routing by
		// centroids, say). Nothing to decode; aggregation is still defined.
		if len(s.Schedule) != 0 {
			return fmt.Errorf("%w: a space without axes has no schedule", ErrSchema)
		}
		return nil
	}
	for i, a := range s.Axes {
		if math.IsNaN(a.Min) || math.IsInf(a.Min, 0) || math.IsNaN(a.Max) || math.IsInf(a.Max, 0) || !(a.Min < a.Max) {
			return fmt.Errorf("%w: axis %d needs finite min < max", ErrSchema, i)
		}
	}
	if len(s.Schedule) != bitsPerChar*width {
		return fmt.Errorf("%w: the schedule has %d bits, the address %d", ErrSchema, len(s.Schedule), bitsPerChar*width)
	}
	for _, a := range s.Schedule {
		if int(a) >= len(s.Axes) {
			return fmt.Errorf("%w: the schedule names axis %d of %d", ErrSchema, a, len(s.Axes))
		}
	}
	cuts := s.BitsAt(bitsPerChar, width)
	for i, n := range cuts {
		if n == 0 {
			return fmt.Errorf("%w: axis %d is never cut", ErrSchema, i)
		}
	}
	for i, n := range s.BitsAt(bitsPerChar, recordDepth) {
		if d := s.Axes[i].DyadicLevel(); n > d {
			return fmt.Errorf("%w: axis %d is cut %d times at depth %d but its bounds are exact for %d cuts only", ErrSchema, i, n, recordDepth, d)
		}
	}
	return nil
}

// Step reports how many times each axis is cut inside every character when
// that number is the same for all axes and all characters: the case the SDK
// walk handles today.
func (s Space) Step(bitsPerChar int) (int, bool) {
	k := len(s.Axes)
	if k == 0 || bitsPerChar%k != 0 {
		return 0, false
	}
	for c := 0; c*bitsPerChar < len(s.Schedule); c++ {
		count := make([]int, k)
		for n := 0; n < bitsPerChar; n++ {
			count[s.Schedule[c*bitsPerChar+n]]++
		}
		for _, x := range count {
			if x != bitsPerChar/k {
				return 0, false
			}
		}
	}
	return bitsPerChar / k, true
}

// OrderedAxis reports the axis along which address order is the order of
// that axis below a depth: it exists when every bit below the depth cuts one
// and the same axis. Ordered columns (first, last, affine) follow
// that axis only then; otherwise they follow the interleaved (Morton) order.
func (s Space) OrderedAxis(bitsPerChar, depth int) (int, bool) {
	if depth*bitsPerChar >= len(s.Schedule) {
		return 0, false
	}
	a := s.Schedule[depth*bitsPerChar]
	for _, b := range s.Schedule[depth*bitsPerChar:] {
		if b != a {
			return 0, false
		}
	}
	return int(a), true
}

func alphabetIndex(c byte) int { return strings.IndexByte(nameAlphabet, c) }

// rot is the rotation of level l: the index of the name's character, taken
// modulo the base (2^bitsPerChar characters), so that every level is a
// rotation of the base's own characters.
func rot(name string, l, size int) int { return alphabetIndex(name[l%len(name)]) % size }

// EncodeLocation places a point (one coordinate per axis) at a depth.
// Points outside the root box are refused.
func (s Space) EncodeLocation(bitsPerChar int, name string, x []float64, depth int) (string, error) {
	if len(s.Axes) == 0 {
		return "", ErrOpaque
	}
	if len(x) != len(s.Axes) {
		return "", fmt.Errorf("%w: %d coordinates for %d axes", ErrSchema, len(x), len(s.Axes))
	}
	lo, hi := make([]float64, len(x)), make([]float64, len(x))
	for i, a := range s.Axes {
		if !(x[i] >= a.Min && x[i] <= a.Max) {
			return "", fmt.Errorf("%w: coordinate %d = %v outside [%v, %v]", ErrSchema, i, x[i], a.Min, a.Max)
		}
		lo[i], hi[i] = a.Min, a.Max
	}
	if depth == 0 {
		return "@", nil
	}
	size := 1 << bitsPerChar
	var b strings.Builder
	for l := 0; l < depth; l++ {
		idx := 0
		for n := 0; n < bitsPerChar; n++ {
			a := s.Schedule[l*bitsPerChar+n]
			mid := (lo[a] + hi[a]) / 2
			if x[a] > mid {
				idx, lo[a] = idx*2+1, mid
			} else {
				idx, hi[a] = idx*2, mid
			}
		}
		b.WriteByte(nameAlphabet[(idx+rot(name, l, size))%size])
	}
	return b.String(), nil
}

// CellOf returns the integer cell of a location: j_a and n_a per axis.
// This is the exact, platform-free view; it involves no float arithmetic.
func (s Space) CellOf(bitsPerChar int, name, location string) (j []uint64, n []int, err error) {
	if len(s.Axes) == 0 {
		return nil, nil, ErrOpaque
	}
	j, n = make([]uint64, len(s.Axes)), make([]int, len(s.Axes))
	if location == "@" {
		return j, n, nil
	}
	size := 1 << bitsPerChar
	for l := 0; l < len(location); l++ {
		c := alphabetIndex(location[l])
		if c < 0 || c >= size {
			return nil, nil, fmt.Errorf("%w: character %q", ErrSchema, location[l])
		}
		idx := (c - rot(name, l, size) + size) % size
		for b := 0; b < bitsPerChar; b++ {
			a := s.Schedule[l*bitsPerChar+b]
			if n[a] == 64 {
				return nil, nil, fmt.Errorf("%w: axis %d cut more than 64 times: no integer cell", ErrSchema, a)
			}
			j[a] = j[a]<<1 | uint64((idx>>(bitsPerChar-1-b))&1)
			n[a]++
		}
	}
	return j, n, nil
}

// Box returns the bounds of an integer cell, flat [min, max] per axis:
// min_a + j Δ_a / 2^n and the next boundary, exact while n <= DyadicLevel.
func (s Space) Box(j []uint64, n []int) []float64 {
	box := make([]float64, 2*len(s.Axes))
	for a, ax := range s.Axes {
		lo, hi := ax.Min, ax.Max
		for b := n[a] - 1; b >= 0; b-- { // replay the bisection, most significant bit first
			mid := (lo + hi) / 2
			if (j[a]>>uint(b))&1 == 1 {
				lo = mid
			} else {
				hi = mid
			}
		}
		box[2*a], box[2*a+1] = lo, hi
	}
	return box
}

// DecodeLocation returns the box of a zone, flat [min, max] per axis.
// It bisects directly, so it also decodes axes cut more than 64 times.
func (s Space) DecodeLocation(bitsPerChar int, name, location string) ([]float64, error) {
	if len(s.Axes) == 0 {
		return nil, ErrOpaque
	}
	box := make([]float64, 2*len(s.Axes))
	for a, ax := range s.Axes {
		box[2*a], box[2*a+1] = ax.Min, ax.Max
	}
	if location == "@" {
		return box, nil
	}
	size := 1 << bitsPerChar
	for l := 0; l < len(location); l++ {
		c := alphabetIndex(location[l])
		if c < 0 || c >= size || l*bitsPerChar >= len(s.Schedule) {
			return nil, fmt.Errorf("%w: character %q at %d", ErrSchema, location[l], l)
		}
		idx := (c - rot(name, l, size) + size) % size
		for b := 0; b < bitsPerChar; b++ {
			a := s.Schedule[l*bitsPerChar+b]
			mid := (box[2*a] + box[2*a+1]) / 2
			if (idx>>(bitsPerChar-1-b))&1 == 1 {
				box[2*a] = mid
			} else {
				box[2*a+1] = mid
			}
		}
	}
	return box, nil
}

// ChildRank is the geometric rank of a child character at a level: the
// order ordered columns must use. The rotation makes it differ from the
// order of the characters, hence from the storage order of location strings.
func ChildRank(bitsPerChar int, name string, level int, c byte) int {
	size := 1 << bitsPerChar
	return (alphabetIndex(c) - rot(name, level, size) + size) % size
}

// ErrOpaque reports a coordinate operation on a space without axes: its
// addresses are chosen by the writer and decode to nothing.
var ErrOpaque = errors.New("algebra: the space has no axes; addresses are opaque")

// CellOrder is the key that sorts locations in cell order: one byte per
// character, its rank with the rotation removed. A prefix sorts before its
// extensions (a record placed at a zone before the records below it), and
// bytes.Compare on two keys is the order ordered columns fold in. The root
// "@" is the empty key.
func CellOrder(bitsPerChar int, name, location string) []byte {
	if location == "@" {
		return nil
	}
	size := 1 << bitsPerChar
	out := make([]byte, len(location))
	for l := range out {
		out[l] = byte((alphabetIndex(location[l]) - rot(name, l, size) + size) % size)
	}
	return out
}
