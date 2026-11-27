package testoracle

import "bytes"

const Size = 12

// ID is a 96-bit identifier. Convert to the engine type with indexus.ID(x).
type ID [Size]byte

func bit(id ID, i uint8) uint8 {
	if int(i) >= Size*8 {
		return 0
	}
	by := i / 8
	mask := uint8(1 << (7 - i%8))
	if id[by]&mask != 0 {
		return 1
	}
	return 0
}

func setBit(id ID, i uint8, side uint8) ID {
	out := id
	if int(i) >= Size*8 {
		return out
	}
	by := i / 8
	mask := byte(1 << (7 - i%8))
	if side&1 == 1 {
		out[by] |= mask
	} else {
		out[by] &^= mask
	}
	return out
}

// Packed puts n into the high `bits` of a 96-bit ID; the rest is zero.
func Packed(n uint64, bits uint8) ID {
	var id ID
	if bits == 0 {
		return id
	}
	if bits > 64 {
		bits = 64
	}
	for i := uint8(0); i < bits; i++ {
		shift := bits - 1 - i
		if (n>>shift)&1 == 1 {
			id = setBit(id, i, 1)
		}
	}
	return id
}

func xorBytes(a, b ID) []byte {
	out := make([]byte, Size)
	for i := 0; i < Size; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func asID[T ~[12]byte](id T) ID { return ID(id) }

// XORCompare materializes both distances then bytes.Compare. No production
// early-exit index.
func XORCompare[T ~[12]byte](key, a, b T) int {
	k, x, y := asID(key), asID(a), asID(b)
	return bytes.Compare(xorBytes(k, x), xorBytes(k, y))
}

// OwnerSlow is owner(z)=argmin XOR. Linear scan, first-wins on ties.
func OwnerSlow[T ~[12]byte](key T, active []T) T {
	if len(active) == 0 {
		var zero T
		return zero
	}
	best := active[0]
	for _, v := range active[1:] {
		if XORCompare(key, v, best) < 0 {
			best = v
		}
	}
	return best
}

// LCPSlow counts leading equal bits one by one.
func LCPSlow[T ~[12]byte](a, b T) uint8 {
	x, y := asID(a), asID(b)
	var n uint8
	for i := uint8(0); i < uint8(Size*8); i++ {
		if bit(x, i) != bit(y, i) {
			return n
		}
		n++
	}
	return n
}

// GuardSlow is g(D)=1+max_{U≠D} LCP(D,U), or 0 if D is alone.
// When max LCP is 95, g(D)=96 (no safe fork bit).
func GuardSlow[T ~[12]byte](donor T, others []T) uint8 {
	var max uint8
	found := false
	for _, u := range others {
		if asID(u) == asID(donor) {
			continue
		}
		l := LCPSlow(donor, u)
		if !found || l > max {
			max = l
			found = true
		}
	}
	if !found {
		return 0
	}
	if int(max)+1 > Size*8 {
		return uint8(Size * 8)
	}
	return max + 1
}

type Change[T ~[12]byte] struct {
	Zone T
	Old  T
	New  T
}

// OwnershipDiff is the brute-force set of owner changes. It does not
// consult Topology or any planner prediction.
func OwnershipDiff[T ~[12]byte](oldView, newView []T, zones []T) []Change[T] {
	var out []Change[T]
	for _, z := range zones {
		o, n := OwnerSlow(z, oldView), OwnerSlow(z, newView)
		if asID(o) != asID(n) {
			out = append(out, Change[T]{Zone: z, Old: o, New: n})
		}
	}
	return out
}
