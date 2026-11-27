package main

// lire-tree: LIRE on a tree of centroids, as Indexus zones would hold it.
// Every zone has a count and sum columns, so its centroid is sum / count
// whatever it holds (records for a leaf, the records of its subtree for an
// inner zone: Indexus summaries). A zone has at most `fanout` children.
//
//	write   descend from the root to the child with the nearest centroid,
//	        down to a leaf (a posting)
//	split   a leaf over capacity: 2-means, the new leaf becomes a sibling;
//	        LIRE reassignment among the nearest sibling leaves; an inner
//	        zone over the fan-out splits its children by 2-means on their
//	        centroids (a new level when it is the root)
//	merge   a leaf under capacity / 8 gives its vectors to its nearest
//	        sibling leaves; an inner zone with one child is replaced by it
//	search  best-first descent by centroid distance until nprobe leaves

import (
	"container/heap"
	"math"
	"math/rand"
	"sort"
)

var fanout = 64

// growDown: the Indexus rule. Addresses are paths, so nothing above a split
// may move: a leaf splits into a sibling while its parent has room, else into
// two children of its own; an inner zone never splits, and a zone left with
// one child keeps it (no collapse). Only the split leaf's records move.
var growDown = false

type tnode struct {
	parent   *tnode
	children []*tnode
	z        *zone // the leaf's records; for an inner zone, its sums only
	count    int
	cent     vec
	dirty    bool
}

func (n *tnode) leaf() bool { return n.children == nil }

func (n *tnode) centroid() vec {
	if n.dirty && n.count > 0 {
		for i, s := range n.z.sum {
			n.cent[i] = float32(s / float64(n.count))
		}
		n.dirty = false
	}
	return n.cent
}

type ltree struct {
	root      *tnode
	where     map[int]*tnode // entry -> leaf
	moves     int
	neighbors int
	rng       *rand.Rand
	compared  int // centroid comparisons on writes
}

func newNode(parent *tnode, leaf bool) *tnode {
	n := &tnode{parent: parent, z: newZone(), cent: make(vec, dim), dirty: true}
	if !leaf {
		n.children = []*tnode{}
	}
	return n
}

func newTree(rng *rand.Rand) *ltree {
	return &ltree{root: newNode(nil, true), where: map[int]*tnode{}, neighbors: 8, rng: rng}
}

// bubble adds sign*v to the sums of n and its ancestors.
func bubble(n *tnode, v vec, sign int) {
	for ; n != nil; n = n.parent {
		n.count += sign
		for i, x := range v {
			n.z.sum[i] += float64(sign) * float64(x)
		}
		n.dirty = true
	}
}

func (t *ltree) put(leaf *tnode, e int) {
	leaf.z.ents = append(leaf.z.ents, e)
	bubble(leaf, data[e>>1], 1)
	t.where[e] = leaf
}

func (t *ltree) take(leaf *tnode, e int) {
	ents := leaf.z.ents
	found := false
	for i, x := range ents {
		if x == e {
			ents[i] = ents[len(ents)-1]
			leaf.z.ents = ents[:len(ents)-1]
			found = true
			break
		}
	}
	if !found {
		panic("take: entry not in its leaf")
	}
	bubble(leaf, data[e>>1], -1)
	delete(t.where, e)
}

func (t *ltree) descend(v vec) *tnode {
	n := t.root
	for !n.leaf() {
		best, bd := n.children[0], float32(math.Inf(1))
		for _, c := range n.children {
			if c.count == 0 {
				continue
			}
			t.compared++
			if d := dist(v, c.centroid()); d < bd {
				best, bd = c, d
			}
		}
		n = best
	}
	return n
}

func (t *ltree) insert(id int) {
	leaf := t.descend(data[id])
	t.put(leaf, id<<1)
	if len(leaf.z.ents) > *capacity {
		t.split(leaf)
	}
}

func (t *ltree) siblingsLeaves(n *tnode) []*tnode {
	if n.parent == nil {
		return nil
	}
	var out []*tnode
	for _, c := range n.parent.children {
		if c != n && c.leaf() && c.count > 0 {
			out = append(out, c)
		}
	}
	return out
}

func (t *ltree) nearestOf(v vec, among []*tnode) *tnode {
	var best *tnode
	bd := float32(math.Inf(1))
	for _, c := range among {
		if d := dist(v, c.centroid()); d < bd {
			best, bd = c, d
		}
	}
	return best
}

func (t *ltree) split(leaf *tnode) {
	ents := append([]int(nil), leaf.z.ents...)
	ca := append(vec(nil), data[ents[t.rng.Intn(len(ents))]>>1]...)
	cb := append(vec(nil), data[ents[t.rng.Intn(len(ents))]>>1]...)
	var left, right []int
	for it := 0; it < 6; it++ {
		left, right = nil, nil
		for _, e := range ents {
			if dist(data[e>>1], ca) <= dist(data[e>>1], cb) {
				left = append(left, e)
			} else {
				right = append(right, e)
			}
		}
		if len(left) == 0 || len(right) == 0 {
			left, right = append([]int(nil), ents[:len(ents)/2]...), append([]int(nil), ents[len(ents)/2:]...)
		}
		ca, cb = meanOf(left), meanOf(right)
	}
	if growDown && (leaf.parent == nil || len(leaf.parent.children) >= fanout) {
		t.splitDown(leaf, left, right)
		return
	}
	if leaf.parent == nil { // the root leaf: grow a level
		root := newNode(nil, false)
		root.count, root.z.sum = leaf.count, append([]float64(nil), leaf.z.sum...)
		root.dirty = true
		root.children = []*tnode{leaf}
		leaf.parent = root
		t.root = root
	}
	sib := newNode(leaf.parent, true)
	leaf.parent.children = append(leaf.parent.children, sib)
	for _, e := range right {
		t.take(leaf, e)
		t.put(sib, e)
		t.moves++
	}
	// LIRE: vectors of the nearest sibling leaves now nearer to a new centroid.
	sibs := t.siblingsLeaves(leaf)
	sort.Slice(sibs, func(i, j int) bool {
		return dist(leaf.centroid(), sibs[i].centroid()) < dist(leaf.centroid(), sibs[j].centroid())
	})
	if len(sibs) > t.neighbors {
		sibs = sibs[:t.neighbors]
	}
	cands := append(append([]*tnode(nil), sibs...), leaf)
	for _, n := range cands {
		if n == sib {
			continue
		}
		for _, e := range append([]int(nil), n.z.ents...) {
			v := data[e>>1]
			best := t.nearestOf(v, append(cands, sib))
			if best != n && dist(v, best.centroid()) < dist(v, n.centroid()) {
				t.take(n, e)
				t.put(best, e)
				t.moves++
			}
		}
	}
	if len(leaf.parent.children) > fanout {
		t.splitInner(leaf.parent)
	}
	for _, n := range append(cands, sib) {
		if n.leaf() && len(n.z.ents) > *capacity {
			t.split(n)
		}
	}
}

// splitDown turns a leaf into an inner zone with two leaf children.
func (t *ltree) splitDown(leaf *tnode, left, right []int) {
	all := append(append([]int(nil), left...), right...)
	for _, e := range all {
		t.take(leaf, e)
	}
	leaf.children = []*tnode{}
	leaf.z.ents = nil
	a, b := newNode(leaf, true), newNode(leaf, true)
	leaf.children = append(leaf.children, a, b)
	for _, e := range left {
		t.put(a, e)
		t.moves++
	}
	for _, e := range right {
		t.put(b, e)
		t.moves++
	}
	for _, n := range []*tnode{a, b} {
		if len(n.z.ents) > *capacity {
			t.split(n)
		}
	}
}

// splitInner: an inner zone over the fan-out splits its children in two
// groups by 2-means on their centroids.
func (t *ltree) splitInner(n *tnode) {
	kids := n.children
	ca := append(vec(nil), kids[t.rng.Intn(len(kids))].centroid()...)
	cb := append(vec(nil), kids[t.rng.Intn(len(kids))].centroid()...)
	var a, b []*tnode
	for it := 0; it < 6; it++ {
		a, b = nil, nil
		for _, k := range kids {
			if dist(k.centroid(), ca) <= dist(k.centroid(), cb) {
				a = append(a, k)
			} else {
				b = append(b, k)
			}
		}
		if len(a) == 0 || len(b) == 0 {
			a, b = append([]*tnode(nil), kids[:len(kids)/2]...), append([]*tnode(nil), kids[len(kids)/2:]...)
		}
		ca, cb = meanNodes(a), meanNodes(b)
	}
	if n.parent == nil {
		root := newNode(nil, false)
		root.count, root.z.sum = n.count, append([]float64(nil), n.z.sum...)
		root.dirty = true
		root.children = []*tnode{n}
		n.parent = root
		t.root = root
	}
	m := newNode(n.parent, false)
	n.parent.children = append(n.parent.children, m)
	n.children = append([]*tnode(nil), a...)
	for _, k := range b {
		k.parent = m
		m.children = append(m.children, k)
		// move k's sums from n to m (the ancestors above both are unchanged)
		n.count -= k.count
		m.count += k.count
		for i, s := range k.z.sum {
			n.z.sum[i] -= s
			m.z.sum[i] += s
		}
	}
	n.dirty, m.dirty = true, true
	if len(n.parent.children) > fanout {
		t.splitInner(n.parent)
	}
}

func meanNodes(ns []*tnode) vec {
	s := make([]float64, dim)
	total := 0
	for _, k := range ns {
		total += k.count
		for i, x := range k.z.sum {
			s[i] += x
		}
	}
	c := make(vec, dim)
	if total == 0 {
		return c
	}
	for i := range c {
		c[i] = float32(s[i] / float64(total))
	}
	return c
}

func (t *ltree) delete(id int) {
	leaf, ok := t.where[id<<1]
	if !ok {
		return
	}
	t.take(leaf, id<<1)
	if len(leaf.z.ents) >= *capacity/8 || leaf.parent == nil {
		return
	}
	sibs := t.siblingsLeaves(leaf)
	if len(sibs) == 0 {
		return
	}
	for _, e := range append([]int(nil), leaf.z.ents...) {
		best := t.nearestOf(data[e>>1], sibs)
		t.take(leaf, e)
		t.put(best, e)
		t.moves++
	}
	t.detach(leaf)
	for _, s := range sibs {
		if len(s.z.ents) > *capacity {
			t.split(s)
			break
		}
	}
}

// detach removes an empty zone; an inner zone left with one child is
// replaced by it.
func (t *ltree) detach(n *tnode) {
	p := n.parent
	for i, c := range p.children {
		if c == n {
			p.children = append(p.children[:i], p.children[i+1:]...)
			break
		}
	}
	if len(p.children) == 0 && p.parent != nil {
		if growDown {
			p.children = nil // an empty inner zone becomes an empty leaf again
			return
		}
		t.detach(p)
		return
	}
	if len(p.children) == 1 && !growDown {
		only := p.children[0]
		if p.parent == nil {
			only.parent = nil
			t.root = only
			return
		}
		g := p.parent
		for i, c := range g.children {
			if c == p {
				g.children[i] = only
			}
		}
		only.parent = g
	}
}

func (t *ltree) leaves(n *tnode, out *[]*tnode) {
	if n.leaf() {
		if len(n.z.ents) > 0 {
			*out = append(*out, n)
		}
		return
	}
	for _, c := range n.children {
		t.leaves(c, out)
	}
}

func (t *ltree) zones() []*zone {
	var ls []*tnode
	t.leaves(t.root, &ls)
	out := make([]*zone, len(ls))
	for i, l := range ls {
		l.z.cent = l.centroid()
		out[i] = l.z
	}
	return out
}
func (t *ltree) moved() int { return t.moves }

func (t *ltree) depth() int {
	var deepest func(n *tnode) int
	deepest = func(n *tnode) int {
		d := 0
		for _, c := range n.children {
			d = max(d, 1+deepest(c))
		}
		return d
	}
	return deepest(t.root)
}

// ---- best-first search over the tree ----

type item struct {
	n *tnode
	d float32
}
type pq []item

func (p pq) Len() int           { return len(p) }
func (p pq) Less(i, j int) bool { return p[i].d < p[j].d }
func (p pq) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p *pq) Push(x any)        { *p = append(*p, x.(item)) }
func (p *pq) Pop() any          { o := *p; it := o[len(o)-1]; *p = o[:len(o)-1]; return it }

// searchTree reads only centroids it meets: no global ranking of postings.
func searchTree(t *ltree, q vec, nprobe int) (map[int]bool, int, int) {
	h := &pq{{t.root, 0}}
	var cs []cand
	scanned, compared, found := 0, 0, 0
	for h.Len() > 0 && found < nprobe {
		it := heap.Pop(h).(item)
		if it.n.leaf() {
			for _, e := range it.n.z.ents {
				cs = append(cs, cand{e >> 1, dist(q, data[e>>1])})
			}
			scanned += len(it.n.z.ents)
			found++
			continue
		}
		for _, c := range it.n.children {
			if c.count == 0 {
				continue
			}
			compared++
			heap.Push(h, item{c, dist(q, c.cent)})
		}
	}
	return top10(cs), scanned, compared
}

// searchBeam: level by level, keep the `beam` nearest zones, open every
// inner one, until only leaves are kept; scan the nprobe nearest of them.
func searchBeam(t *ltree, q vec, nprobe, beam int) (map[int]bool, int, int) {
	type nd struct {
		n *tnode
		d float32
	}
	frontier := []nd{{t.root, 0}}
	compared := 0
	for {
		inner := false
		var next []nd
		for _, f := range frontier {
			if f.n.leaf() {
				next = append(next, f)
				continue
			}
			inner = true
			for _, c := range f.n.children {
				if c.count == 0 {
					continue
				}
				compared++
				next = append(next, nd{c, dist(q, c.cent)})
			}
		}
		sort.Slice(next, func(i, j int) bool { return next[i].d < next[j].d })
		if len(next) > beam {
			next = next[:beam]
		}
		frontier = next
		if !inner {
			break
		}
	}
	var cs []cand
	scanned := 0
	for i := 0; i < nprobe && i < len(frontier); i++ {
		for _, e := range frontier[i].n.z.ents {
			cs = append(cs, cand{e >> 1, dist(q, data[e>>1])})
		}
		scanned += len(frontier[i].n.z.ents)
	}
	return top10(cs), scanned, compared
}
