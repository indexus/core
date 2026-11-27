package convergent

import (
	"maps"
	"slices"
)

// Vector is a version vector with exceptions. For each incarnation it covers
// every counter up to Base, plus isolated counters received out of order. A
// counter after a gap is never covered by Base until the gap fills.
type Vector struct {
	Base  map[Incarnation]uint64
	Extra map[Dot]bool
}

func NewVector() Vector {
	return Vector{Base: map[Incarnation]uint64{}, Extra: map[Dot]bool{}}
}

func (v Vector) Covers(d Dot) bool {
	return d.Counter <= v.Base[d.Incarnation] || v.Extra[d]
}

// Add records one dot and compacts the contiguous prefix.
func (v Vector) Add(d Dot) {
	if v.Covers(d) {
		return
	}
	if injected == VectorTakesMax {
		v.Base[d.Incarnation] = d.Counter
		return
	}
	v.Extra[d] = true
	v.compact(d.Incarnation)
}

func (v Vector) compact(inc Incarnation) {
	for {
		next := Dot{inc, v.Base[inc] + 1}
		if !v.Extra[next] {
			return
		}
		delete(v.Extra, next)
		v.Base[inc] = next.Counter
	}
}

// Merge makes v cover everything o covers. It is only sound for a replica
// whose state already holds the effect of every dot o covers.
func (v Vector) Merge(o Vector) {
	for inc, c := range o.Base {
		if c > v.Base[inc] {
			v.Base[inc] = c
		}
	}
	for d := range o.Extra {
		if !v.Covers(d) {
			v.Extra[d] = true
		}
	}
	for d := range v.Extra {
		if d.Counter <= v.Base[d.Incarnation] {
			delete(v.Extra, d)
		}
	}
	for _, inc := range slices.Collect(maps.Keys(v.Base)) {
		v.compact(inc)
	}
	for d := range v.Extra {
		v.compact(d.Incarnation)
	}
}

// LessOrEqual reports whether v covers nothing o does not.
func (v Vector) LessOrEqual(o Vector) bool {
	for inc, c := range v.Base {
		if c > o.Base[inc] {
			// o may cover the tail through exceptions.
			for n := o.Base[inc] + 1; n <= c; n++ {
				if !o.Extra[Dot{inc, n}] {
					return false
				}
			}
		}
	}
	for d := range v.Extra {
		if !o.Covers(d) {
			return false
		}
	}
	return true
}

func (v Vector) Clone() Vector {
	return Vector{Base: maps.Clone(v.Base), Extra: maps.Clone(v.Extra)}
}

func (v Vector) Equal(o Vector) bool {
	return v.LessOrEqual(o) && o.LessOrEqual(v)
}
