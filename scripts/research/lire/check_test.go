package main

import (
	"math/rand"
	"testing"
)

func TestTreeInvariants(t *testing.T) {
	for _, down := range []bool{false, true} {
		growDown = down
		treeInvariants(t)
	}
	growDown = false
}

func treeInvariants(t *testing.T) {
	*capacity = 64
	rng := rand.New(rand.NewSource(1))
	base, _ := synthetic(rng, 8000, 1)
	data, dim = base, len(base[0])
	tr := newTree(rand.New(rand.NewSource(2)))
	live := map[int]bool{}
	check := func(step int) {
		var ls []*tnode
		tr.leaves(tr.root, &ls)
		seen := map[int]bool{}
		var walk func(n *tnode) int
		walk = func(n *tnode) int {
			if n.leaf() {
				if n.count != len(n.z.ents) {
					t.Fatalf("step %d: leaf count %d, %d entries", step, n.count, len(n.z.ents))
				}
				for _, e := range n.z.ents {
					if seen[e] {
						t.Fatalf("step %d: entry %d twice", step, e)
					}
					seen[e] = true
					if tr.where[e] != n {
						t.Fatalf("step %d: where of %d is not its leaf", step, e)
					}
				}
				return n.count
			}
			s := 0
			for _, c := range n.children {
				if c.parent != n {
					t.Fatalf("step %d: wrong parent", step)
				}
				s += walk(c)
			}
			if s != n.count {
				t.Fatalf("step %d: inner count %d, children %d", step, n.count, s)
			}
			return s
		}
		walk(tr.root)
		for id := range live {
			if !seen[id<<1] {
				t.Fatalf("step %d: live %d missing", step, id)
			}
		}
		if len(seen) != len(live) {
			t.Fatalf("step %d: %d entries, %d live", step, len(seen), len(live))
		}
	}
	r := rand.New(rand.NewSource(3))
	var ids []int
	for i := range base {
		tr.insert(i)
		live[i] = true
		ids = append(ids, i)
		if i > 1000 && i%3 == 0 {
			j := r.Intn(len(ids))
			id := ids[j]
			ids[j] = ids[len(ids)-1]
			ids = ids[:len(ids)-1]
			tr.delete(id)
			delete(live, id)
		}
		if i%97 == 0 {
			check(i)
		}
	}
	check(-1)
}
