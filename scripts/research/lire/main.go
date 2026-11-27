// Benchmark: Indexus zones as SPFresh postings maintained by LIRE, on public
// vector sets (fvecs files) or synthetic clusters.
//
// Indexes compared, all searched the same way (rank zones by centroid =
// sum / count, scan the nprobe nearest, keep the 10 nearest vectors):
//
//	ivf       static reference: k-means over the final live vectors, as many
//	          postings as LIRE ends with (FAISS IVFFlat, built once, no
//	          freshness: its best case)
//	fixed     the Indexus encoding as it is: 16 principal axes, bisection of
//	          fixed bounds, 2 bits per character, capacity splits and folds
//	lire      opaque addresses: nearest-centroid placement, 2-means splits,
//	          LIRE reassignment, merges
//	lire+rep  lire, and a vector also goes to its second posting when that
//	          one is almost as near (SPFresh boundary replication)
//
// The stream: every base vector inserted in file order, and after the first
// quarter one delete of a random live vector every 5 inserts.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	baseFile  = flag.String("base", "", "base vectors (.fvecs); empty: synthetic clusters")
	queryFile = flag.String("queries", "", "query vectors (.fvecs); empty: held out from the base file")
	limit     = flag.Int("n", 60000, "vectors inserted")
	nq        = flag.Int("q", 500, "queries")
	capacity  = flag.Int("cap", 256, "zone capacity")
	seed      = flag.Int64("seed", 1, "seed")
	spread    = flag.Float64("spread", 1.0, "synthetic: cluster spread")
	clusters  = flag.Int("clusters", 1000, "synthetic: clusters (half as many appear halfway)")
	synthDim  = flag.Int("d", 64, "synthetic: dimensions")
	only      = flag.String("only", "", "run only these variants (comma list)")
	fan       = flag.Int("fanout", 64, "tree: children per zone at most")
	down      = flag.Bool("growdown", false, "tree: grow only downward (Indexus addresses never move)")
	trees     = flag.String("trees", "", "tree configurations, comma list of fanout[d] (d: grow down), e.g. 64,64d,16d; empty: -fanout and -growdown")
	repEps    = flag.Float64("eps", 0.15, "lire+rep: replicate when d2 <= (1+eps)^2 d1")
)

type vec []float32

func dist(a, b vec) float32 {
	var s0, s1, s2, s3 float32
	n := len(a) &^ 3
	for i := 0; i < n; i += 4 {
		x0, x1, x2, x3 := a[i]-b[i], a[i+1]-b[i+1], a[i+2]-b[i+2], a[i+3]-b[i+3]
		s0 += x0 * x0
		s1 += x1 * x1
		s2 += x2 * x2
		s3 += x3 * x3
	}
	for i := n; i < len(a); i++ {
		x := a[i] - b[i]
		s0 += x * x
	}
	return s0 + s1 + s2 + s3
}

var data []vec
var dim int

// ---------- input ----------

func readFvecs(path string, max int) []vec {
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	var out []vec
	var head [4]byte
	for max <= 0 || len(out) < max {
		if _, err := io.ReadFull(f, head[:]); err != nil {
			break
		}
		d := int(binary.LittleEndian.Uint32(head[:]))
		buf := make([]byte, 4*d)
		if _, err := io.ReadFull(f, buf); err != nil {
			panic(err)
		}
		v := make(vec, d)
		for i := range v {
			v[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[4*i:]))
		}
		out = append(out, v)
	}
	return out
}

func synthetic(rng *rand.Rand, n, q int) ([]vec, []vec) {
	var centers []vec
	add := func(k int) {
		for i := 0; i < k; i++ {
			c := make(vec, *synthDim)
			for j := range c {
				c[j] = float32(rng.NormFloat64())
			}
			centers = append(centers, c)
		}
	}
	sample := func() vec {
		c := centers[rng.Intn(len(centers))]
		v := make(vec, len(c))
		for j := range v {
			v[j] = c[j] + float32(*spread*rng.NormFloat64())
		}
		return v
	}
	add(*clusters)
	var base []vec
	for i := 0; i < n; i++ {
		if i == n/2 {
			add(*clusters / 2)
		}
		base = append(base, sample())
	}
	qs := make([]vec, q)
	for i := range qs {
		qs[i] = sample()
	}
	return base, qs
}

// ---------- zones ----------

// A zone holds entries: id<<1 | copy (copy 1 is a boundary replica).
type zone struct {
	ents []int
	sum  []float64
	cent vec
}

func newZone() *zone { return &zone{sum: make([]float64, dim), cent: make(vec, dim)} }

func (z *zone) add(e int) {
	z.ents = append(z.ents, e)
	for i, x := range data[e>>1] {
		z.sum[i] += float64(x)
	}
}

func (z *zone) remove(e int) bool {
	for i, x := range z.ents {
		if x == e {
			z.ents[i] = z.ents[len(z.ents)-1]
			z.ents = z.ents[:len(z.ents)-1]
			for j, y := range data[e>>1] {
				z.sum[j] -= float64(y)
			}
			return true
		}
	}
	return false
}

// refresh recomputes the centroid from the sum columns.
func (z *zone) refresh() {
	if len(z.ents) == 0 {
		return
	}
	n := float64(len(z.ents))
	for i, s := range z.sum {
		z.cent[i] = float32(s / n)
	}
}

type index interface {
	insert(id int)
	delete(id int)
	zones() []*zone
	moved() int
}

func meanOf(ents []int) vec {
	s := make([]float64, dim)
	for _, e := range ents {
		for j, y := range data[e>>1] {
			s[j] += float64(y)
		}
	}
	c := make(vec, dim)
	for j := range c {
		c[j] = float32(s[j] / float64(len(ents)))
	}
	return c
}

// ---------- fixed: the Indexus encoding as it is ----------

type fixed struct {
	c, k   int
	proj   [][]float32
	lo, hi []float64
	leaves map[string]*zone
	where  map[int]string
	bits   map[int]string
	moves  int
}

func newFixed(k, c int, rng *rand.Rand, sample []vec) *fixed {
	f := &fixed{c: c, k: k, leaves: map[string]*zone{}, where: map[int]string{}, bits: map[int]string{}}
	f.proj = principal(sample, k, rng)
	f.lo, f.hi = make([]float64, k), make([]float64, k)
	for a := 0; a < k; a++ {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, v := range sample {
			x := f.coord(v, a)
			lo, hi = math.Min(lo, x), math.Max(hi, x)
		}
		w := hi - lo
		f.lo[a], f.hi[a] = lo-w, hi+w
	}
	f.leaves[""] = newZone()
	return f
}

func (f *fixed) coord(v vec, a int) float64 {
	var s float64
	for j, x := range v {
		s += float64(x * f.proj[a][j])
	}
	return s
}

func (f *fixed) address(v vec) string {
	lo, hi := append([]float64(nil), f.lo...), append([]float64(nil), f.hi...)
	x := make([]float64, f.k)
	for a := range x {
		x[a] = math.Max(f.lo[a], math.Min(f.hi[a], f.coord(v, a)))
	}
	var b strings.Builder
	for bit := 0; bit < 60; bit++ {
		a := bit % f.k
		mid := (lo[a] + hi[a]) / 2
		if x[a] > mid {
			b.WriteByte('1')
			lo[a] = mid
		} else {
			b.WriteByte('0')
			hi[a] = mid
		}
	}
	return b.String()
}

func suffixes(c int) []string {
	var out []string
	for i := 0; i < 1<<c; i++ {
		out = append(out, fmt.Sprintf("%0*b", c, i))
	}
	return out
}

func (f *fixed) insert(id int) {
	addr := f.address(data[id])
	f.bits[id] = addr
	leaf := ""
	for p := 0; p <= len(addr); p += f.c {
		if _, ok := f.leaves[addr[:p]]; ok {
			leaf = addr[:p]
			break
		}
	}
	z := f.leaves[leaf]
	z.add(id << 1)
	f.where[id] = leaf
	if len(z.ents) > *capacity && len(leaf)+f.c <= 60 {
		delete(f.leaves, leaf)
		for _, s := range suffixes(f.c) {
			f.leaves[leaf+s] = newZone()
		}
		for _, e := range z.ents {
			child := f.bits[e>>1][:len(leaf)+f.c]
			f.leaves[child].add(e)
			f.where[e>>1] = child
			f.moves++
		}
	}
}

func (f *fixed) delete(id int) {
	leaf := f.where[id]
	f.leaves[leaf].remove(id << 1)
	delete(f.where, id)
	if leaf == "" {
		return
	}
	parent := leaf[:len(leaf)-f.c]
	total := 0
	for _, s := range suffixes(f.c) {
		z, ok := f.leaves[parent+s]
		if !ok {
			return
		}
		total += len(z.ents)
	}
	if total*2 > *capacity {
		return
	}
	merged := newZone()
	for _, s := range suffixes(f.c) {
		for _, e := range f.leaves[parent+s].ents {
			merged.add(e)
			f.where[e>>1] = parent
			f.moves++
		}
		delete(f.leaves, parent+s)
	}
	f.leaves[parent] = merged
}

func (f *fixed) zones() []*zone {
	var out []*zone
	for _, z := range f.leaves {
		if len(z.ents) > 0 {
			z.refresh()
			out = append(out, z)
		}
	}
	return out
}
func (f *fixed) moved() int { return f.moves }

// principal: the k leading principal axes of the sample.
func principal(sample []vec, k int, rng *rand.Rand) [][]float32 {
	d := dim
	mean := make([]float64, d)
	for _, v := range sample {
		for j, x := range v {
			mean[j] += float64(x) / float64(len(sample))
		}
	}
	cov := make([][]float64, d)
	for i := range cov {
		cov[i] = make([]float64, d)
	}
	for _, v := range sample {
		c := make([]float64, d)
		for j := range c {
			c[j] = float64(v[j]) - mean[j]
		}
		for i := 0; i < d; i++ {
			for j := i; j < d; j++ {
				cov[i][j] += c[i] * c[j]
			}
		}
	}
	for i := 0; i < d; i++ {
		for j := 0; j < i; j++ {
			cov[i][j] = cov[j][i]
		}
	}
	var out [][]float32
	for a := 0; a < k; a++ {
		u := make([]float64, d)
		for j := range u {
			u[j] = rng.NormFloat64()
		}
		var lambda float64
		for it := 0; it < 60; it++ {
			w := make([]float64, d)
			for i := 0; i < d; i++ {
				var s float64
				for j := 0; j < d; j++ {
					s += cov[i][j] * u[j]
				}
				w[i] = s
			}
			var n float64
			for _, x := range w {
				n += x * x
			}
			n = math.Sqrt(n)
			lambda = n
			for i := range u {
				u[i] = w[i] / n
			}
		}
		for i := 0; i < d; i++ {
			for j := 0; j < d; j++ {
				cov[i][j] -= lambda * u[i] * u[j]
			}
		}
		row := make([]float32, d)
		for i := range row {
			row[i] = float32(u[i])
		}
		out = append(out, row)
	}
	return out
}

// ---------- lire ----------

type lire struct {
	posts     []*zone // nil: merged away
	where     map[int][]int
	moves     int
	neighbors int
	replicate bool
	rng       *rand.Rand
}

func newLire(replicate bool, rng *rand.Rand) *lire {
	return &lire{where: map[int][]int{}, neighbors: 8, replicate: replicate, rng: rng}
}

// nearest two live postings of v, in parallel chunks when there are many.
func (l *lire) nearest2(v vec) (int, float32, int, float32) {
	if len(l.posts) >= 1024 {
		w := runtime.NumCPU()
		type best struct {
			b1, b2 int
			d1, d2 float32
		}
		res := make([]best, w)
		var wg sync.WaitGroup
		chunk := (len(l.posts) + w - 1) / w
		for k := 0; k < w; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				r := best{-1, -1, float32(math.Inf(1)), float32(math.Inf(1))}
				for p := k * chunk; p < min(len(l.posts), (k+1)*chunk); p++ {
					z := l.posts[p]
					if z == nil {
						continue
					}
					d := dist(v, z.cent)
					if d < r.d1 {
						r.b2, r.d2, r.b1, r.d1 = r.b1, r.d1, p, d
					} else if d < r.d2 {
						r.b2, r.d2 = p, d
					}
				}
				res[k] = r
			}(k)
		}
		wg.Wait()
		b1, b2 := -1, -1
		d1, d2 := float32(math.Inf(1)), float32(math.Inf(1))
		for _, r := range res {
			for _, c := range []struct {
				p int
				d float32
			}{{r.b1, r.d1}, {r.b2, r.d2}} {
				if c.p < 0 {
					continue
				}
				if c.d < d1 {
					b2, d2, b1, d1 = b1, d1, c.p, c.d
				} else if c.d < d2 {
					b2, d2 = c.p, c.d
				}
			}
		}
		return b1, d1, b2, d2
	}
	b1, b2 := -1, -1
	d1, d2 := float32(math.Inf(1)), float32(math.Inf(1))
	for p, z := range l.posts {
		if z == nil {
			continue
		}
		d := dist(v, z.cent)
		if d < d1 {
			b2, d2 = b1, d1
			b1, d1 = p, d
		} else if d < d2 {
			b2, d2 = p, d
		}
	}
	return b1, d1, b2, d2
}

func (l *lire) nearestAmong(v vec, among []int) int {
	best, bd := -1, float32(math.Inf(1))
	for _, p := range among {
		if l.posts[p] == nil {
			continue
		}
		if d := dist(v, l.posts[p].cent); d < bd {
			best, bd = p, d
		}
	}
	return best
}

func (l *lire) nearestPosts(c vec, k int) []int {
	type pd struct {
		p int
		d float32
	}
	var all []pd
	for p, z := range l.posts {
		if z != nil {
			all = append(all, pd{p, dist(c, z.cent)})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].d < all[j].d })
	var out []int
	for i := 0; i < len(all) && i < k; i++ {
		out = append(out, all[i].p)
	}
	return out
}

func (l *lire) put(p, e int) {
	l.posts[p].add(e)
	l.where[e] = append(l.where[e], p)
}

func (l *lire) take(p, e int) {
	l.posts[p].remove(e)
	ps := l.where[e]
	for i, q := range ps {
		if q == p {
			ps[i] = ps[len(ps)-1]
			ps = ps[:len(ps)-1]
			break
		}
	}
	if len(ps) == 0 {
		delete(l.where, e)
	} else {
		l.where[e] = ps
	}
}

func (l *lire) insert(id int) {
	v := data[id]
	if len(l.posts) == 0 {
		l.posts = append(l.posts, newZone())
		copy(l.posts[0].cent, v)
	}
	p, d1, q, d2 := l.nearest2(v)
	l.put(p, id<<1)
	l.posts[p].refresh()
	eps := 1 + *repEps
	if l.replicate && q >= 0 && float64(d2) <= eps*eps*float64(d1) {
		l.put(q, id<<1|1)
		l.posts[q].refresh()
	}
	for _, z := range []int{p, q} {
		if z >= 0 && l.posts[z] != nil && len(l.posts[z].ents) > *capacity {
			l.split(z)
		}
	}
}

func (l *lire) split(p int) {
	ents := append([]int(nil), l.posts[p].ents...)
	ca := append(vec(nil), data[ents[l.rng.Intn(len(ents))]>>1]...)
	cb := append(vec(nil), data[ents[l.rng.Intn(len(ents))]>>1]...)
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
	old := append(vec(nil), l.posts[p].cent...)
	for _, e := range ents {
		l.take(p, e)
	}
	l.posts = append(l.posts, newZone())
	q := len(l.posts) - 1
	for _, e := range left {
		l.put(p, e)
	}
	for _, e := range right {
		l.put(q, e)
		l.moves++
	}
	l.posts[p].refresh()
	l.posts[q].refresh()
	near := l.nearestPosts(old, l.neighbors)
	cands := append(append([]int(nil), near...), p, q)
	for _, n := range near {
		if n == p || n == q {
			continue
		}
		for _, e := range append([]int(nil), l.posts[n].ents...) {
			best := l.nearestAmong(data[e>>1], cands)
			if best != n && best >= 0 && dist(data[e>>1], l.posts[best].cent) < dist(data[e>>1], l.posts[n].cent) {
				l.take(n, e)
				l.put(best, e)
				l.moves++
			}
		}
		l.posts[n].refresh()
	}
	l.posts[p].refresh()
	l.posts[q].refresh()
	for _, z := range cands {
		if l.posts[z] != nil && len(l.posts[z].ents) > *capacity {
			l.split(z)
		}
	}
}

func (l *lire) delete(id int) {
	for _, e := range []int{id << 1, id<<1 | 1} {
		for _, p := range append([]int(nil), l.where[e]...) {
			l.take(p, e)
			l.posts[p].refresh()
			l.mergeIfSmall(p)
		}
	}
}

func (l *lire) mergeIfSmall(p int) {
	z := l.posts[p]
	if z == nil || len(z.ents) >= *capacity/8 || l.live() < 2 {
		return
	}
	ents := append([]int(nil), z.ents...)
	cent := append(vec(nil), z.cent...)
	for _, e := range ents {
		l.take(p, e)
	}
	l.posts[p] = nil
	near := l.nearestPosts(cent, l.neighbors)
	for _, e := range ents {
		best := l.nearestAmong(data[e>>1], near)
		l.put(best, e)
		l.posts[best].refresh()
		l.moves++
	}
	for _, n := range near {
		if l.posts[n] != nil && len(l.posts[n].ents) > *capacity {
			l.split(n)
		}
	}
}

func (l *lire) live() int {
	n := 0
	for _, z := range l.posts {
		if z != nil {
			n++
		}
	}
	return n
}

func (l *lire) zones() []*zone {
	var out []*zone
	for _, z := range l.posts {
		if z != nil && len(z.ents) > 0 {
			out = append(out, z)
		}
	}
	return out
}
func (l *lire) moved() int { return l.moves }

// ---------- ivf: static k-means reference ----------

type ivf struct{ zs []*zone }

func buildIVF(live []int, k int, rng *rand.Rand) *ivf {
	cents := make([]vec, k)
	for i := range cents {
		cents[i] = append(vec(nil), data[live[rng.Intn(len(live))]]...)
	}
	// Trained on a sample of 40 vectors per centroid, as FAISS recommends
	// (at least 39), 6 Lloyd iterations; then every vector is assigned.
	all := live
	if len(live) > 40*k {
		sample := make([]int, 40*k)
		for i := range sample {
			sample[i] = live[rng.Intn(len(live))]
		}
		live = sample
	}
	assign := make([]int, len(live))
	nearestCent := func(v vec) int {
		best, bd := 0, float32(math.Inf(1))
		for c, ce := range cents {
			if d := dist(v, ce); d < bd {
				best, bd = c, d
			}
		}
		return best
	}
	for it := 0; it < 6; it++ {
		parallel(len(live), func(i int) { assign[i] = nearestCent(data[live[i]]) })
		sums := make([][]float64, k)
		counts := make([]int, k)
		for c := range sums {
			sums[c] = make([]float64, dim)
		}
		for i, id := range live {
			c := assign[i]
			counts[c]++
			for j, x := range data[id] {
				sums[c][j] += float64(x)
			}
		}
		for c := range cents {
			if counts[c] == 0 {
				cents[c] = append(vec(nil), data[live[rng.Intn(len(live))]]...)
				continue
			}
			for j := range cents[c] {
				cents[c][j] = float32(sums[c][j] / float64(counts[c]))
			}
		}
	}
	final := make([]int, len(all))
	parallel(len(all), func(i int) { final[i] = nearestCent(data[all[i]]) })
	x := &ivf{}
	for range cents {
		x.zs = append(x.zs, newZone())
	}
	for i, id := range all {
		x.zs[final[i]].add(id << 1)
	}
	return x
}

func (x *ivf) insert(int) {}
func (x *ivf) delete(int) {}
func (x *ivf) moved() int { return 0 }
func (x *ivf) zones() []*zone {
	var out []*zone
	for _, z := range x.zs {
		if len(z.ents) > 0 {
			z.refresh()
			out = append(out, z)
		}
	}
	return out
}

// ---------- search ----------

func parallel(n int, f func(i int)) {
	workers := runtime.NumCPU()
	var wg sync.WaitGroup
	next := make(chan int, 1024)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
}

type cand struct {
	id int
	d  float32
}

func top10(cs []cand) map[int]bool {
	sort.Slice(cs, func(i, j int) bool { return cs[i].d < cs[j].d })
	out := map[int]bool{}
	for _, c := range cs {
		if len(out) == 10 {
			break
		}
		out[c.id] = true
	}
	return out
}

func search(zs []*zone, q vec, nprobe int) (map[int]bool, int) {
	type zd struct {
		z *zone
		d float32
	}
	ranked := make([]zd, len(zs))
	for i, z := range zs {
		ranked[i] = zd{z, dist(q, z.cent)}
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].d < ranked[j].d })
	var cs []cand
	scanned := 0
	for i := 0; i < nprobe && i < len(ranked); i++ {
		for _, e := range ranked[i].z.ents {
			cs = append(cs, cand{e >> 1, dist(q, data[e>>1])})
		}
		scanned += len(ranked[i].z.ents)
	}
	return top10(cs), scanned
}

func main() {
	flag.Parse()
	rng := rand.New(rand.NewSource(*seed))
	var base, qs []vec
	label := "synthetic"
	if *baseFile == "" {
		base, qs = synthetic(rng, *limit, *nq)
	} else {
		label = *baseFile
		if *queryFile != "" {
			base = readFvecs(*baseFile, *limit)
			qs = readFvecs(*queryFile, *nq)
		} else {
			want := 0
			if *limit > 0 {
				want = *limit + *nq
			}
			all := readFvecs(*baseFile, want)
			base, qs = all[:len(all)-*nq], all[len(all)-*nq:]
		}
	}
	data = base
	dim = len(base[0])

	type op struct {
		del bool
		id  int
	}
	var ops []op
	var live []int
	drng := rand.New(rand.NewSource(*seed + 1))
	for i := range base {
		ops = append(ops, op{false, i})
		live = append(live, i)
		if i > len(base)/4 && i%5 == 0 {
			j := drng.Intn(len(live))
			id := live[j]
			live[j] = live[len(live)-1]
			live = live[:len(live)-1]
			ops = append(ops, op{true, id})
		}
	}
	started := time.Now()
	gts := make([]map[int]bool, len(qs))
	parallel(len(qs), func(i int) {
		cs := make([]cand, len(live))
		for k, id := range live {
			cs[k] = cand{id, dist(qs[i], data[id])}
		}
		gts[i] = top10(cs)
	})
	fmt.Printf("%s: d=%d inserts=%d deletes=%d live=%d capacity=%d queries=%d (truth %s)\n",
		label, dim, len(base), len(ops)-len(base), len(live), *capacity, len(qs), time.Since(started).Round(time.Millisecond))

	run := func(name string, ix index, build time.Duration) {
		zs := ix.zones()
		sizes := make([]int, len(zs))
		entries := 0
		for i, z := range zs {
			sizes[i] = len(z.ents)
			entries += len(z.ents)
		}
		sort.Ints(sizes)
		fmt.Printf("\n%-9s zones=%d size p50=%d p90=%d max=%d entries/live=%.2f moved/insert=%.2f build=%s\n", name, len(zs),
			sizes[len(sizes)/2], sizes[len(sizes)*9/10], sizes[len(sizes)-1], float64(entries)/float64(len(live)),
			float64(ix.moved())/float64(len(base)), build.Round(time.Millisecond))
		for _, nprobe := range []int{1, 2, 4, 8, 16, 32, 64, 128} {
			hits := make([]int, len(qs))
			scans := make([]int, len(qs))
			parallel(len(qs), func(i int) {
				got, s := search(zs, qs[i], nprobe)
				scans[i] = s
				for id := range got {
					if gts[i][id] {
						hits[i]++
					}
				}
			})
			hit, scanned := 0, 0
			for i := range qs {
				hit += hits[i]
				scanned += scans[i]
			}
			fmt.Printf("  nprobe=%-4d recall@10=%.3f  scanned/query=%7.0f (%5.2f%% of live)\n", nprobe,
				float64(hit)/float64(10*len(qs)), float64(scanned)/float64(len(qs)), 100*float64(scanned)/float64(len(qs))/float64(len(live)))
		}
	}
	stream := func(ix index) time.Duration {
		t := time.Now()
		for _, o := range ops {
			if o.del {
				ix.delete(o.id)
			} else {
				ix.insert(o.id)
			}
		}
		return time.Since(t)
	}
	want := func(name string) bool {
		if *only == "" {
			return true
		}
		for _, w := range strings.Split(*only, ",") {
			if w == name {
				return true
			}
		}
		return false
	}
	postings := 0
	if want("lire") {
		l := newLire(false, rand.New(rand.NewSource(*seed+3)))
		t := stream(l)
		postings = len(l.zones())
		if want("lire") {
			run("lire", l, t)
		}
	}
	configs := strings.Split(*trees, ",")
	if *trees == "" {
		configs = []string{fmt.Sprintf("%d%s", *fan, map[bool]string{true: "d"}[*down])}
	}
	for _, cfg := range configs {
		if !want("tree") {
			break
		}
		fanout, growDown = 64, strings.HasSuffix(cfg, "d")
		fmt.Sscanf(strings.TrimSuffix(cfg, "d"), "%d", &fanout)
		fmt.Printf("\n##### tree fanout=%d growdown=%v\n", fanout, growDown)
		tr := newTree(rand.New(rand.NewSource(*seed + 5)))
		t := stream(tr)
		if postings == 0 {
			postings = len(tr.zones())
		}
		// centroids of inner zones are read in search without refresh: refresh all once.
		var walk func(n *tnode)
		walk = func(n *tnode) {
			n.centroid()
			for _, c := range n.children {
				walk(c)
			}
		}
		walk(tr.root)
		fmt.Printf("\ntree: depth=%d, centroid comparisons per insert=%.0f\n", tr.depth(), float64(tr.compared)/float64(len(base)))
		run("lire-tree", tr, t)
		for _, mult := range []int{1, 2, 4, 8} {
			fmt.Printf("  (beam descent, beam = %d x nprobe)\n", mult)
			for _, nprobe := range []int{8, 16, 32, 64} {
				hits := make([]int, len(qs))
				scans := make([]int, len(qs))
				comps := make([]int, len(qs))
				parallel(len(qs), func(i int) {
					got, s, c := searchBeam(tr, qs[i], nprobe, mult*nprobe)
					scans[i], comps[i] = s, c
					for id := range got {
						if gts[i][id] {
							hits[i]++
						}
					}
				})
				hit, scanned, compared := 0, 0, 0
				for i := range qs {
					hit += hits[i]
					scanned += scans[i]
					compared += comps[i]
				}
				fmt.Printf("  nprobe=%-4d recall@10=%.3f  scanned/query=%7.0f (%5.2f%% of live)  centroids/query=%.0f\n", nprobe,
					float64(hit)/float64(10*len(qs)), float64(scanned)/float64(len(qs)), 100*float64(scanned)/float64(len(qs))/float64(len(live)), float64(compared)/float64(len(qs)))
			}
		}
		fmt.Printf("  (best-first descent instead of ranking every posting)\n")
		for _, nprobe := range []int{4, 16, 32, 64, 128} {
			hits := make([]int, len(qs))
			scans := make([]int, len(qs))
			comps := make([]int, len(qs))
			parallel(len(qs), func(i int) {
				got, s, c := searchTree(tr, qs[i], nprobe)
				scans[i], comps[i] = s, c
				for id := range got {
					if gts[i][id] {
						hits[i]++
					}
				}
			})
			hit, scanned, compared := 0, 0, 0
			for i := range qs {
				hit += hits[i]
				scanned += scans[i]
				compared += comps[i]
			}
			fmt.Printf("  nprobe=%-4d recall@10=%.3f  scanned/query=%7.0f (%5.2f%% of live)  centroids/query=%.0f\n", nprobe,
				float64(hit)/float64(10*len(qs)), float64(scanned)/float64(len(qs)), 100*float64(scanned)/float64(len(qs))/float64(len(live)), float64(compared)/float64(len(qs)))
		}
	}
	if want("lire+rep") {
		l := newLire(true, rand.New(rand.NewSource(*seed+3)))
		run("lire+rep", l, stream(l))
	}
	if want("ivf") {
		if postings == 0 {
			postings = max(1, len(live)/190)
		}
		t := time.Now()
		x := buildIVF(live, postings, rand.New(rand.NewSource(*seed+4)))
		run("ivf", x, time.Since(t))
	}
	if want("fixed") {
		sample := data[:min(2000, len(data))]
		f := newFixed(16, 2, rand.New(rand.NewSource(*seed+2)), sample)
		run("fixed", f, stream(f))
	}
}
