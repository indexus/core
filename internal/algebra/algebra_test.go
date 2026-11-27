package algebra

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// Every kind once, plus a second affine pair.
var full = Schema{Sum, Min, Max, First, Last, AffineA, AffineB, Sum, AffineA, AffineB}

func randomRecord(rng *rand.Rand, s Schema) Value {
	m := make([]float64, len(s))
	for i, k := range s {
		switch k {
		case AffineA:
			m[i] = 0.5 + 0.5*rng.Float64() // a decay in (0.5, 1)
		default:
			m[i] = math.Round(rng.NormFloat64()*1000) / 8 // exact in binary: sums stay exact
		}
	}
	return s.Lift(m)
}

// close compares with a relative tolerance on affine columns only; every
// other column must match exactly (records are multiples of 1/8, so sums of
// a few thousand of them are exact in float64).
func close(s Schema, a, b Value) bool {
	if a.Count != b.Count {
		return false
	}
	if a.Count == 0 {
		return true
	}
	for i, k := range s {
		x, y := a.Cols[i], b.Cols[i]
		if k == AffineA || k == AffineB {
			if math.Abs(x-y) > 1e-9*(1+math.Abs(x)+math.Abs(y)) {
				return false
			}
		} else if x != y {
			return false
		}
	}
	return true
}

func TestValidate(t *testing.T) {
	if err := full.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Schema{{AffineA}, {AffineB, Sum}, {AffineA, Sum}, {Kind(42)}} {
		if bad.Validate() == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestIdentityAndAssociativity(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	z := full.Zero()
	for n := 0; n < 2000; n++ {
		a, b, c := randomRecord(rng, full), randomRecord(rng, full), randomRecord(rng, full)
		if !Equal(full.Combine(z, a), a) || !Equal(full.Combine(a, z), a) {
			t.Fatalf("zero is not the identity")
		}
		left := full.Combine(full.Combine(a, b), c)
		right := full.Combine(a, full.Combine(b, c))
		if !close(full, left, right) {
			t.Fatalf("not associative:\n%v\n%v", left, right)
		}
	}
}

func TestOrderedKindsAreNotCommutative(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for _, s := range []Schema{{First}, {Last}, {AffineA, AffineB}} {
		differs := false
		for n := 0; n < 100 && !differs; n++ {
			a, b := randomRecord(rng, s), randomRecord(rng, s)
			differs = !close(s, s.Combine(a, b), s.Combine(b, a))
		}
		if !differs || s.Commutative() {
			t.Errorf("%v: expected order to matter", s)
		}
	}
	if !(Schema{Sum, Min, Max}).Commutative() {
		t.Errorf("sum/min/max are commutative")
	}
}

// A small address tree: base 4, depth 5, records at random leaves and at
// random interior zones (records placed exactly at a zone come first).
type node struct {
	loc      string
	direct   []Value
	children map[byte]*node
}

const alphabet = "0123"

func buildTree(rng *rand.Rand, s Schema, records int) (*node, []Value) {
	root := &node{children: map[byte]*node{}}
	type placed struct {
		loc string
		v   Value
	}
	var all []placed
	for i := 0; i < records; i++ {
		depth := rng.Intn(6)
		var b strings.Builder
		for d := 0; d < depth; d++ {
			b.WriteByte(alphabet[rng.Intn(4)])
		}
		loc := b.String()
		v := randomRecord(rng, s)
		all = append(all, placed{loc, v})
		n := root
		for d := 0; d < len(loc); d++ {
			c := loc[d]
			next := n.children[c]
			if next == nil {
				next = &node{loc: loc[:d+1], children: map[byte]*node{}}
				n.children[c] = next
			}
			n = next
		}
		n.direct = append(n.direct, v)
	}
	// Address order: a prefix before its extensions, then by character; ties
	// at one location in insertion order.
	sort.SliceStable(all, func(i, j int) bool { return all[i].loc < all[j].loc })
	ordered := make([]Value, len(all))
	for i, p := range all {
		ordered[i] = p.v
	}
	return root, ordered
}

func (n *node) summary(s Schema) Value {
	acc := s.Fold(n.direct...)
	for i := 0; i < len(alphabet); i++ {
		if c := n.children[alphabet[i]]; c != nil {
			acc = s.Combine(acc, c.summary(s))
		}
	}
	return acc
}

// randomCut reads a random antichain covering the subtree, in address order:
// at each zone, either take its summary or open it (direct records, then
// children in order).
func (n *node) randomCut(rng *rand.Rand, s Schema, reads *int) Value {
	if len(n.children) == 0 || rng.Intn(3) == 0 {
		*reads++
		return n.summary(s)
	}
	acc := s.Fold(n.direct...)
	for i := 0; i < len(alphabet); i++ {
		if c := n.children[alphabet[i]]; c != nil {
			acc = s.Combine(acc, c.randomCut(rng, s, reads))
		}
	}
	return acc
}

func TestFoldInvariantAndCutIndependence(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for trial := 0; trial < 50; trial++ {
		root, ordered := buildTree(rng, full, 1+rng.Intn(800))
		want := full.Fold(ordered...) // the homomorphism, record by record in address order
		if got := root.summary(full); !close(full, got, want) {
			t.Fatalf("fold invariant broken")
		}
		for cut := 0; cut < 20; cut++ {
			reads := 0
			if got := root.randomCut(rng, full, &reads); !close(full, got, want) {
				t.Fatalf("trial %d: a cut of %d zones disagrees with the root", trial, reads)
			}
		}
	}
}

func TestSemilatticeColumnsAreExactInAnyOrder(t *testing.T) {
	// Min/max do not depend on the order at all, bit for bit; float sums do.
	// This is why an audit needs a tolerance for sums and none for min/max.
	rng := rand.New(rand.NewSource(4))
	s := Schema{Sum, Min, Max}
	vals := make([]Value, 5000)
	for i := range vals {
		vals[i] = s.Lift([]float64{rng.NormFloat64() * 1e6, rng.NormFloat64(), rng.NormFloat64()})
	}
	ref := s.Fold(vals...)
	sumDiffers := false
	for k := 0; k < 20; k++ {
		rng.Shuffle(len(vals), func(i, j int) { vals[i], vals[j] = vals[j], vals[i] })
		got := s.Fold(vals...)
		if got.Cols[1] != ref.Cols[1] || got.Cols[2] != ref.Cols[2] {
			t.Fatalf("min/max changed with order")
		}
		sumDiffers = sumDiffers || got.Cols[0] != ref.Cols[0]
	}
	t.Logf("float sum bitwise order-dependent: %v (expected true; Neumaier bounds the error, it does not remove it)", sumDiffers)
}

func TestRegisterConvergesUnderDuplicatesReorderingAndStaleness(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	children := []string{"0", "1", "2", "3"}
	for trial := 0; trial < 200; trial++ {
		// Each child writes a history of versions; its single writer numbers them.
		var msgs []Contribution
		latest := map[string]Contribution{}
		for _, c := range children {
			if rng.Intn(5) == 0 {
				continue // a child that never pushed
			}
			versions := 1 + rng.Intn(6)
			for v := 1; v <= versions; v++ {
				m := Contribution{Child: c, Version: uint64(v), Value: randomRecord(rng, full)}
				msgs = append(msgs, m)
				latest[c] = m
			}
		}
		// The network duplicates, reorders, and delivers to two replicas of
		// the parent's state that later merge.
		var a, b []Contribution
		for _, m := range msgs {
			for k := 0; k < 1+rng.Intn(3); k++ {
				if rng.Intn(2) == 0 {
					a = append(a, m)
				} else {
					b = append(b, m)
				}
			}
		}
		rng.Shuffle(len(a), func(i, j int) { a[i], a[j] = a[j], a[i] })
		rng.Shuffle(len(b), func(i, j int) { b[i], b[j] = b[j], b[i] })
		ra, rb := Register{}, Register{}
		for _, m := range a {
			ra.Accept(m)
		}
		for _, m := range b {
			rb.Accept(m)
		}
		// Make sure every message reached at least one side, as eventual delivery promises.
		for _, m := range msgs {
			ra.Accept(m)
		}
		direct := randomRecord(rng, full)
		var want []Value
		want = append(want, direct)
		for _, c := range children {
			if l, ok := latest[c]; ok {
				want = append(want, l.Value)
			}
		}
		ab, ba := Merge(ra, rb), Merge(rb, ra)
		if !Equal(ab.Value(full, children, direct), ba.Value(full, children, direct)) {
			t.Fatalf("merge is not commutative")
		}
		if !Equal(Merge(ab, ab).Value(full, children, direct), ab.Value(full, children, direct)) {
			t.Fatalf("merge is not idempotent")
		}
		if got := ab.Value(full, children, direct); !close(full, got, full.Fold(want...)) {
			t.Fatalf("trial %d: register did not converge to the fold of the newest versions", trial)
		}
	}
}

func TestRemoveRescansRarelyForMax(t *testing.T) {
	// Removing records one by one in random order: a max column needs a
	// rescan only when the removed record held the max, about ln(n) times.
	rng := rand.New(rand.NewSource(6))
	s := Schema{Sum, Max}
	n := 10000
	vals := make([]Value, n)
	for i := range vals {
		vals[i] = s.Lift([]float64{1, rng.Float64()})
	}
	total := s.Fold(vals...)
	rescans := 0
	for len(vals) > 0 {
		i := rng.Intn(len(vals))
		removed := vals[i]
		vals[i] = vals[len(vals)-1]
		vals = vals[:len(vals)-1]
		next, rescan := s.Remove(total, removed)
		if len(rescan) > 0 {
			rescans++
			next = s.Fold(vals...)
		}
		if want := s.Fold(vals...); !Equal(next, want) {
			t.Fatalf("remove disagrees with a recompute")
		}
		total = next
	}
	bound := 3*math.Log(float64(n)) + 5
	t.Logf("%d removals, %d rescans (ln n = %.1f)", n, rescans, math.Log(float64(n)))
	if float64(rescans) > bound {
		t.Fatalf("too many rescans: %d > %.0f", rescans, bound)
	}
}

func TestAffinePrefixFromTreeMatchesSequentialScan(t *testing.T) {
	// The X6 result in Go: the state of h = a*h + b after any prefix, read
	// as O(log n) dyadic segment summaries, equals the sequential scan.
	rng := rand.New(rand.NewSource(7))
	s := Schema{AffineA, AffineB}
	n := 1 << 12
	leaf := make([]Value, n)
	for i := range leaf {
		leaf[i] = s.Lift([]float64{0.9 + 0.099*rng.Float64(), rng.NormFloat64()})
	}
	levels := [][]Value{leaf}
	for len(levels[len(levels)-1]) > 1 {
		prev := levels[len(levels)-1]
		next := make([]Value, len(prev)/2)
		for j := range next {
			next[j] = s.Combine(prev[2*j], prev[2*j+1])
		}
		levels = append(levels, next)
	}
	maxReads := 0
	h := 0.0
	for t0 := 1; t0 <= n; t0++ {
		h = leaf[t0-1].Cols[0]*h + leaf[t0-1].Cols[1]
		acc, pos, reads := s.Zero(), 0, 0
		for k := len(levels) - 1; k >= 0; k-- {
			if t0&(1<<k) != 0 {
				acc = s.Combine(acc, levels[k][pos>>k])
				pos += 1 << k
				reads++
			}
		}
		if reads > maxReads {
			maxReads = reads
		}
		if math.Abs(acc.Cols[1]-h) > 1e-9*(1+math.Abs(h)) {
			t.Fatalf("prefix %d: tree %v, scan %v", t0, acc.Cols[1], h)
		}
	}
	t.Logf("n = %d, at most %d zone reads per prefix", n, maxReads)
}

func FuzzAssociative(f *testing.F) {
	f.Add(1.0, 2.0, 3.0, 0.5, 0.7, 0.9)
	s := Schema{Sum, Min, Max, First, Last, AffineA, AffineB}
	f.Fuzz(func(t *testing.T, x, y, z, a1, a2, a3 float64) {
		for _, v := range []float64{x, y, z, a1, a2, a3} {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e6 {
				return
			}
		}
		mk := func(v, a float64) Value { return s.Lift([]float64{v, v, v, v, v, a, v}) }
		p, q, r := mk(x, a1), mk(y, a2), mk(z, a3)
		left := s.Combine(s.Combine(p, q), r)
		right := s.Combine(p, s.Combine(q, r))
		for i, k := range s {
			d := math.Abs(left.Cols[i] - right.Cols[i])
			tol := 0.0
			if k == Sum || k == AffineA || k == AffineB {
				tol = 1e-9 * (1 + math.Abs(left.Cols[i]))
			}
			if d > tol {
				t.Fatalf("column %d (%v): %v vs %v", i, k, left.Cols[i], right.Cols[i])
			}
		}
	})
}

func BenchmarkCombine(b *testing.B) {
	for _, s := range []Schema{
		{Sum, Sum, Sum, Sum, Sum, Sum, Sum, Sum},
		{Sum, Min, Max, Sum, Min, Max, Sum, Sum},
		{AffineA, AffineB, AffineA, AffineB, AffineA, AffineB, AffineA, AffineB},
	} {
		rng := rand.New(rand.NewSource(8))
		x, y := randomRecord(rng, s), randomRecord(rng, s)
		b.Run(fmt.Sprint(s), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				x = s.Combine(x, y)
			}
		})
	}
}
