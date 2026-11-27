// Package algebra is the summary algebra of Indexus: how the value of a zone
// is computed from the values of its children, column by column.
//
// ROLE
//
//	A zone summary is the fold, in address order, of the summaries of its
//	children and of the records placed exactly at the zone. Today every
//	column is a sum (an abelian group). This package generalises the column
//	to a monoid, declared per column by a Kind:
//
//	  Sum         abelian group: removal is a delta, no rescan
//	  Min, Max    semilattice: idempotent and commutative; removal rescans
//	              only when the removed value was the extreme
//	  First, Last ordered monoid: value of the first / last record in
//	              address order
//	  Affine      ordered monoid on a pair of columns (a, b):
//	              (a1, b1) then (a2, b2) = (a1*a2, a2*b1 + b2), the state of
//	              a linear recurrence h = a*h + b over the records in order
//
// MODEL
//
//	Combine is associative and has an identity (Zero). It is NOT commutative
//	for First, Last and Affine: callers must combine in address order. With
//	that order, the value of any cut of the tree equals the value of the
//	root (cut independence), which is what lets a client stop at any depth
//	and lets vnodes split the tree anywhere.
//
// NON-GOALS
//
//	No I/O and no other internal package: the standard library only.
package algebra

import (
	"errors"
	"fmt"
	"math"
)

// Kind says how one column of a summary combines.
type Kind uint8

const (
	Sum Kind = iota
	Min
	Max
	First
	Last
	AffineA // the multiplier of an affine pair; the next column must be AffineB
	AffineB // the offset of an affine pair
)

func (k Kind) String() string {
	return [...]string{"sum", "min", "max", "first", "last", "affine.a", "affine.b"}[k]
}

// Invertible reports whether a removal can be applied as a delta.
func (k Kind) Invertible() bool { return k == Sum }

// Commutative reports whether the order of the operands is irrelevant.
func (k Kind) Commutative() bool { return k == Sum || k == Min || k == Max }

// Schema is the kind of every column of a collection.
type Schema []Kind

// ErrSchema reports a malformed schema.
var ErrSchema = errors.New("algebra: invalid schema")

// Validate checks that affine columns come in (AffineA, AffineB) pairs.
func (s Schema) Validate() error {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case AffineA:
			if i+1 >= len(s) || s[i+1] != AffineB {
				return fmt.Errorf("%w: column %d is affine.a without affine.b after it", ErrSchema, i)
			}
			i++
		case AffineB:
			return fmt.Errorf("%w: column %d is affine.b without affine.a before it", ErrSchema, i)
		case Sum, Min, Max, First, Last:
		default:
			return fmt.Errorf("%w: column %d has unknown kind %d", ErrSchema, i, s[i])
		}
	}
	return nil
}

// Commutative reports whether every column is commutative: then children may
// be combined in any order (today's sums, and min/max).
func (s Schema) Commutative() bool {
	for _, k := range s {
		if !k.Commutative() {
			return false
		}
	}
	return true
}

// Value is the summary of a set of records: their count and one value per
// column. Count == 0 is the identity, whatever the columns hold.
type Value struct {
	Count int64
	Cols  []float64
}

// Zero is the identity of Combine for the schema.
func (s Schema) Zero() Value {
	v := Value{Cols: make([]float64, len(s))}
	for i, k := range s {
		switch k {
		case Min:
			v.Cols[i] = math.Inf(1)
		case Max:
			v.Cols[i] = math.Inf(-1)
		case AffineA:
			v.Cols[i] = 1
		}
	}
	return v
}

// Lift is the summary of one record with these metrics.
func (s Schema) Lift(metrics []float64) Value {
	v := Value{Count: 1, Cols: make([]float64, len(s))}
	copy(v.Cols, metrics)
	return v
}

// Combine returns left then right, left preceding right in address order.
func (s Schema) Combine(left, right Value) Value {
	if left.Count == 0 {
		return right.clone()
	}
	if right.Count == 0 {
		return left.clone()
	}
	out := Value{Count: left.Count + right.Count, Cols: make([]float64, len(s))}
	for i := 0; i < len(s); i++ {
		l, r := left.Cols[i], right.Cols[i]
		switch s[i] {
		case Sum:
			out.Cols[i] = l + r
		case Min:
			out.Cols[i] = math.Min(l, r)
		case Max:
			out.Cols[i] = math.Max(l, r)
		case First:
			out.Cols[i] = l
		case Last:
			out.Cols[i] = r
		case AffineA:
			a1, b1, a2, b2 := l, left.Cols[i+1], r, right.Cols[i+1]
			out.Cols[i] = a1 * a2
			// The conversion forbids a fused multiply-add, which Go may emit
			// on arm64: every node and the JS client round the product and
			// the sum separately, so they agree to the last bit.
			out.Cols[i+1] = float64(a2*b1) + b2
			i++
		}
	}
	return out
}

// Fold combines values in the order given.
func (s Schema) Fold(values ...Value) Value {
	acc := s.Zero()
	for _, v := range values {
		acc = s.Combine(acc, v)
	}
	return acc
}

// Remove takes one record out of a total. Columns that cannot be updated
// without the other records (a removed extreme, any ordered column) are
// reported in rescan; their value in the result is meaningless and the
// caller must recompute the zone from its records.
func (s Schema) Remove(total, removed Value) (Value, []int) {
	out := total.clone()
	out.Count -= removed.Count
	var rescan []int
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case Sum:
			out.Cols[i] -= removed.Cols[i]
		case Min, Max:
			if out.Count == 0 {
				out.Cols[i] = s.Zero().Cols[i]
			} else if removed.Cols[i] == total.Cols[i] {
				rescan = append(rescan, i)
			}
		case First, Last:
			if out.Count != 0 {
				rescan = append(rescan, i)
			}
		case AffineA:
			if out.Count != 0 {
				rescan = append(rescan, i, i+1)
			} else {
				out.Cols[i], out.Cols[i+1] = 1, 0
			}
			i++
		}
	}
	return out, rescan
}

// Equal compares two values exactly, column by column. Two NaNs are equal.
func Equal(a, b Value) bool {
	if a.Count != b.Count || len(a.Cols) != len(b.Cols) {
		return false
	}
	if a.Count == 0 {
		return true
	}
	for i := range a.Cols {
		if a.Cols[i] != b.Cols[i] && !(math.IsNaN(a.Cols[i]) && math.IsNaN(b.Cols[i])) {
			return false
		}
	}
	return true
}

func (v Value) clone() Value {
	return Value{Count: v.Count, Cols: append([]float64(nil), v.Cols...)}
}

// Contribution is what a child pushes to its parent: its summary at a version.
// Versions are produced by the single writer of the child, so they increase.
type Contribution struct {
	Child   string
	Version uint64
	Value   Value
}

// Register is the parent's state: the newest contribution of every child.
// Merging two registers is commutative, associative and idempotent (a state
// CRDT), so duplicates, reordering and delays do not change the result.
type Register map[string]Contribution

// Accept keeps the contribution if it is newer than the one held.
func (r Register) Accept(c Contribution) bool {
	held, ok := r[c.Child]
	if ok && held.Version >= c.Version {
		return false
	}
	r[c.Child] = Contribution{Child: c.Child, Version: c.Version, Value: c.Value.clone()}
	return true
}

// Merge returns the join of two registers.
func Merge(a, b Register) Register {
	out := Register{}
	for _, c := range a {
		out.Accept(c)
	}
	for _, c := range b {
		out.Accept(c)
	}
	return out
}

// Value folds the held contributions in child order (address order).
func (r Register) Value(s Schema, order []string, direct Value) Value {
	acc := direct
	for _, child := range order {
		if c, ok := r[child]; ok {
			acc = s.Combine(acc, c.Value)
		}
	}
	return acc
}
