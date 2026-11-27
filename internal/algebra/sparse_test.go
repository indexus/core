package algebra

import (
	"math/rand"
	"sort"
	"testing"
)

// Sparse storage: every item has a full address, but it is held by the zone
// where storage stopped, next to child zones of other cells. A zone folds its
// items and its children in address order: a child zone stands at its
// prefix, an item at its full address. Invariant: an item is never held next
// to the child zone that covers its cell (a split moves all of them down).

type item struct {
	loc string
	v   Value
}

type zone struct {
	items    []item // sorted by address
	children map[byte]*zone
}

// place stores items at depth d: per child cell, either keep them here or
// open a child zone, at random (any storage the nodes might end up with).
func place(rng *rand.Rand, items []item, d, width int) *zone {
	z := &zone{children: map[byte]*zone{}}
	byCell := map[byte][]item{}
	for _, it := range items {
		if d == width {
			z.items = append(z.items, it)
			continue
		}
		byCell[it.loc[d]] = append(byCell[it.loc[d]], it)
	}
	for c, group := range byCell {
		if rng.Intn(2) == 0 {
			z.items = append(z.items, group...)
		} else {
			z.children[c] = place(rng, group, d+1, width)
		}
	}
	sort.SliceStable(z.items, func(i, j int) bool { return z.items[i].loc < z.items[j].loc })
	return z
}

func (z *zone) summary(s Schema, d int) Value {
	acc, k := s.Zero(), 0
	for i := 0; i < len(alphabet); i++ {
		c := alphabet[i]
		for k < len(z.items) && (d == len(z.items[k].loc) || z.items[k].loc[d] <= c) {
			if d < len(z.items[k].loc) && z.items[k].loc[d] == c && z.children[c] != nil {
				panic("item held next to the child zone of its cell")
			}
			acc = s.Combine(acc, z.items[k].v)
			k++
		}
		if ch := z.children[c]; ch != nil {
			acc = s.Combine(acc, ch.summary(s, d+1))
		}
	}
	for ; k < len(z.items); k++ {
		acc = s.Combine(acc, z.items[k].v)
	}
	return acc
}

func TestSparseStorageGivesTheSameValuesWhereverItemsAreHeld(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	const width = 6
	for trial := 0; trial < 200; trial++ {
		items := make([]item, 1+rng.Intn(400))
		for i := range items {
			b := make([]byte, width)
			for d := range b {
				b[d] = alphabet[rng.Intn(4)]
			}
			if i > 0 && rng.Intn(10) == 0 {
				copy(b, items[rng.Intn(i)].loc) // some items share an address
			}
			items[i] = item{string(b), randomRecord(rng, full)}
		}
		sorted := append([]item(nil), items...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].loc < sorted[j].loc })
		vals := make([]Value, len(sorted))
		for i, it := range sorted {
			vals[i] = it.v
		}
		want := full.Fold(vals...)
		for layout := 0; layout < 5; layout++ {
			if got := place(rng, sorted, 0, width).summary(full, 0); !close(full, got, want) {
				t.Fatalf("trial %d layout %d: the root depends on where items are held", trial, layout)
			}
		}
	}
}
