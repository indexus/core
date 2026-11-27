package summary

import (
	"context"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// An owned zone is answered from the store: the sums of its rows, read from the segments,
// corrected by the contributions of children held by other vnodes. No
// summary per zone is kept in memory: only those contributions
// (mesh.incoming, one per remote child) and the boundaries waiting to be
// pushed. A boundary is pushed with the value it has when it is sent.

// incomingBelowLocked lists the remote contributions strictly below a zone
// that are not below another of them: each one's value already holds what
// its subtree sends back to it.
func (t *Tree) incomingBelowLocked(collection, location string) []tree.Key {
	path := summaryPath(location)
	locations := t.incomingIndexLocked()[collection]
	var out []tree.Key
	for i := sort.SearchStrings(locations, path); i < len(locations) && strings.HasPrefix(locations[i], path); i++ {
		at := locations[i]
		if len(at) == len(path) || len(out) > 0 && strings.HasPrefix(at, out[len(out)-1].Location) {
			continue
		}
		out = append(out, tree.Key{Collection: collection, Location: at})
	}
	return out
}

// indexIncomingLocked adds a key about to be added to incoming to
// incomingSorted, or removes one about to be deleted. acceptChild and
// refreshLocked are the only writers of incoming.
func (t *Tree) indexIncomingLocked(key tree.Key, add bool) {
	// Called before incoming changes. If it already disagrees, something
	// changed incoming without it: drop the index, rebuilt at the next read.
	if t.incomingSorted == nil || t.incomingCount != len(t.incoming) {
		t.incomingSorted = nil
		return
	}
	locations := t.incomingSorted[key.Collection]
	i, found := slices.BinarySearch(locations, key.Location)
	switch {
	case add && !found:
		t.incomingSorted[key.Collection] = slices.Insert(locations, i, key.Location)
		t.incomingCount++
	case !add && found:
		t.incomingSorted[key.Collection] = slices.Delete(locations, i, i+1)
		t.incomingCount--
	}
}

// incomingIndexLocked is incomingSorted, rebuilt when it no longer counts
// what incoming holds (first use, or incoming set by other means).
func (t *Tree) incomingIndexLocked() map[string][]string {
	if t.incomingSorted != nil && t.incomingCount == len(t.incoming) {
		return t.incomingSorted
	}
	t.incomingSorted = make(map[string][]string)
	for key := range t.incoming {
		t.incomingSorted[key.Collection] = append(t.incomingSorted[key.Collection], key.Location)
	}
	for _, locations := range t.incomingSorted {
		sort.Strings(locations)
	}
	t.incomingCount = len(t.incoming)
	return t.incomingSorted
}

// storePart is a sum being corrected: the local rows, minus the local rows
// below remote children, plus those children's values.
type storePart struct {
	value  *wire.Abelian
	exact  string
	local  int64 // local records left in it
	remote int   // remote children with records
}

func (p *storePart) correct(local int64, localValue, remote *wire.Abelian, exact string) {
	p.value.Substract(localValue)
	p.local -= local
	if remote != nil && remote.Count() > 0 {
		p.value.Sum(remote)
		p.remote++
		if p.remote == 1 && p.local == 0 {
			p.exact = exact
		} else {
			p.exact = ""
		}
	}
	if p.local == 0 && p.remote == 0 {
		p.exact = ""
	}
}

// zoneSums reads every local sum of one computation from one committed
// version (SUM-4), and keeps the children of the zones already read: a
// zone's total is a slot of its parent's children, and the edges of one
// flush are often siblings (up to 64 read the same parent). A zone's total
// and the totals of the remote children it is corrected with must come from
// one version: otherwise a row a concurrent commit deletes (a collection of
// handed-away rows) or adds (an install) is counted twice or not at all.
// Rows that reach the store without a local write (a bulk, an install) go
// through refreshLocked; currentLocked then moves the sums to the newer
// version. The version is pinned at the first read, not before; close the
// sums when done.
type zoneSums struct {
	tree     *Tree
	view     *storage.ZoneSums
	retired  []*storage.ZoneSums               // replaced under Tree.Mu, closed by close
	children map[tree.Key]*[65]storage.ZoneSum // from childrenPool, back at close
}

var childrenPool = sync.Pool{New: func() any { return new([65]storage.ZoneSum) }}

// childrenReads counts the reads of a parent's 65 sums from the store, for tests.
var childrenReads atomic.Int64

func (t *Tree) openZoneSums() *zoneSums {
	return &zoneSums{tree: t, children: map[tree.Key]*[65]storage.ZoneSum{}}
}

// version is the pinned version, pinned now if no read was made yet.
func (z *zoneSums) version() *storage.ZoneSums {
	if z.view == nil {
		z.view = z.tree.service.Store.OpenZoneSums()
	}
	return z.view
}

// close releases every version the sums pinned. Closing a zonelog snapshot
// can reclaim merged segments (file I/O): call it outside Tree.Mu where the
// caller can.
func (z *zoneSums) close() {
	z.releaseChildren()
	for _, v := range z.retired {
		v.Close()
	}
	z.retired = nil
	if z.view != nil {
		z.view.Close()
		z.view = nil
	}
}

// releaseChildren also runs when a commit invalidates the pinned sums: returning
// arrays only at close would lose the pooled buffers at each version change.
func (z *zoneSums) releaseChildren() {
	for key, children := range z.children {
		*children = [65]storage.ZoneSum{} // drop metrics and exact-location strings
		childrenPool.Put(children)
		delete(z.children, key)
	}
}

// currentLocked moves sums to the current version if the store changed
// without a local write since gen, the storeGen they were read at. It runs
// under Tree.Mu, so the old version is only set aside: close releases it.
func (t *Tree) currentLocked(sums *zoneSums, gen *uint64) {
	if *gen != t.storeGen {
		if sums.view != nil {
			sums.retired = append(sums.retired, sums.view)
			sums.view = nil
		}
		sums.releaseChildren()
		*gen = t.storeGen
	}
}

// zoneTotal is store.ZoneTotal at the version of sums.
func (t *Tree) zoneTotal(sums *zoneSums, collection, location string) (storage.ZoneSum, error) {
	slot := -1
	if len(location) > 0 && location != tree.BASE64.Root() {
		slot = strings.IndexByte(routingAlphabet, location[len(location)-1])
	}
	if slot < 0 {
		return sums.version().Total(collection, location)
	}
	parent := location[:len(location)-1]
	if parent == "" {
		parent = tree.BASE64.Root()
	}
	key := tree.Key{Collection: collection, Location: parent}
	children, ok := sums.children[key]
	if !ok {
		read, err := sums.version().Children(collection, parent)
		if err != nil {
			return storage.ZoneSum{}, err
		}
		childrenReads.Add(1)
		children = childrenPool.Get().(*[65]storage.ZoneSum)
		*children = read
		sums.children[key] = children
	}
	return children[1+slot], nil
}

type incomingEntry struct {
	key   tree.Key
	value *wire.Abelian
	exact string
}

func (t *Tree) snapshotIncomingLocked(collection, location string) []incomingEntry {
	keys := t.incomingBelowLocked(collection, location)
	if len(keys) == 0 {
		return nil
	}
	out := make([]incomingEntry, len(keys))
	for i, key := range keys {
		in := t.incoming[key]
		out[i] = incomingEntry{key: key, value: in.value, exact: in.exact}
	}
	return out
}

// valueWithLocked is valueLocked reading zone totals through sums.
func (t *Tree) valueWithLocked(collection, location string, sums *zoneSums) (*wire.Abelian, string, error) {
	return t.valueWithIncoming(collection, location, sums, t.snapshotIncomingLocked(collection, location))
}

// valueWithIncoming computes a zone's total from the store and a pre-copied
// slice of remote child contributions so callers can release t.Mu during I/O.
// sums nil reads one version of its own.
func (t *Tree) valueWithIncoming(collection, location string, sums *zoneSums, incoming []incomingEntry) (*wire.Abelian, string, error) {
	if c := t.typedCollection(collection); c != nil {
		return t.typedValueWithIncoming(c, collection, location, incoming)
	}
	if sums == nil {
		sums = t.openZoneSums()
		defer sums.close()
	}
	total, err := t.zoneTotal(sums, collection, location)
	if err != nil {
		return nil, "", err
	}
	if t.betweenLocalReads != nil {
		t.betweenLocalReads()
	}
	part := &storePart{value: wire.NewAbelian(int(math.Round(total.Weight)), total.Metrics), exact: total.Only, local: total.Count}
	for _, in := range incoming {
		local, err := t.zoneTotal(sums, collection, in.key.Location)
		if err != nil {
			return nil, "", err
		}
		part.correct(local.Count, wire.NewAbelian(int(math.Round(local.Weight)), local.Metrics), in.value, in.exact)
	}
	if part.value.Count() == 0 {
		part.exact = ""
	}
	return part.value, part.exact, nil
}

// Set answers a read of an owned zone (query.md keys): the records at
// the zone, then one key per child.
func (t *Tree) Set(collection, location string) (*wire.Set, bool) {
	if !t.ReadyFor(collection, location) {
		return nil, false
	}
	t.Mu.Lock()
	// Not before the remote children of this authority are known: after a
	// change of authority, until a full poll of the other hosts has run, a
	// read takes the authoritative path.
	view := t.View()
	if t.Summaries == nil || view == nil || t.Summaries.digest != view.Digest || t.LastFold.IsZero() {
		t.Mu.Unlock()
		return nil, false
	}
	if view.Len() > 1 {
		if digest, err := t.summaryDigest(); err != nil || digest != t.lastDigest {
			t.Mu.Unlock()
			return nil, false
		}
	}
	incoming := t.snapshotIncomingLocked(collection, location)
	t.Mu.Unlock()

	if c := t.typedCollection(collection); c != nil {
		set, err := t.typedSetWithIncoming(c, collection, location, incoming)
		return set, err == nil
	}
	// The children and the totals subtracted from them: one version (SUM-4).
	sums := t.openZoneSums()
	defer sums.close()
	children, err := sums.version().Children(collection, location)
	if err != nil {
		return nil, false
	}

	path := summaryPath(location)
	var parts [65]*storePart
	for i, sum := range children {
		parts[i] = &storePart{value: wire.NewAbelian(int(math.Round(sum.Weight)), sum.Metrics), exact: sum.Only, local: sum.Count}
	}
	for _, in := range incoming {
		slot := 1 + strings.IndexByte(routingAlphabet, in.key.Location[len(path)])
		local, err := t.zoneTotal(sums, collection, in.key.Location)
		if err != nil || slot < 1 {
			return nil, false
		}
		parts[slot].correct(local.Count, wire.NewAbelian(int(math.Round(local.Weight)), local.Metrics), in.value, in.exact)
	}
	set := wire.NewSet()
	if parts[0].value.Count() > 0 {
		set.Put(summaryLocation(path), parts[0].value)
	}
	for c := range 64 {
		if part := parts[1+c]; part.value.Count() > 0 {
			child := path + routingAlphabet[c:c+1]
			set.Put(groupKey(path, child, part.exact), part.value)
		}
	}
	return set, true
}

const routingAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

// boundaries lists the zones of a collection this node holds whose
// parent another vnode holds: what it pushes. The walk goes down only
// where ownership still changes and where this disk has records.
func (t *Tree) boundaries(collection string, view *tree.View) []tree.Key {
	inventory, _ := t.boundaryInventory(view, false)
	return inventory.forCollection(collection)
}

// Identity paths are sufficient to discover candidate boundaries. Empty/tombstone
// candidates are filtered using their live values when sent. No subtree sum is
// needed to decide which parents might need a contribution.
func boundariesIn(collection string, cell tree.Region, inventory storage.ZoneInventory) []tree.Key {
	walk := newBoundaryWalk(collection, cell)
	var out []tree.Key
	_ = inventory.Each(context.Background(), func(z tree.ZoneKey) bool {
		out = walk.add(out, z)
		return true
	})
	return out
}

// boundaryWalk finds the candidate boundaries of zones one at a time. It
// remembers what it decided for each prefix: a boundary is found once.
type boundaryWalk struct {
	collection string
	cell       tree.Region
	// At each location depth, only the cell constraints already fixed by
	// that prefix matter. This is 17 tiny masks, independent of network size.
	prefixes [17]tree.Region
	seen     map[tree.Key]boundaryMark
}

type boundaryMark struct{ mine, stop bool }

func newBoundaryWalk(collection string, cell tree.Region) *boundaryWalk {
	w := &boundaryWalk{collection: collection, cell: cell, seen: map[tree.Key]boundaryMark{}}
	for depth := range w.prefixes {
		for i := range cell.Mask {
			bits := min(max(6*depth-8*i, 0), 8)
			mask := byte(0xff<<uint(8-bits)) & cell.Mask[i]
			w.prefixes[depth].Mask[i] = mask
			w.prefixes[depth].Value[i] = cell.Value[i] & mask
		}
	}
	return w
}

// add appends to out the boundaries z is the first zone to show.
func (w *boundaryWalk) add(out []tree.Key, z tree.ZoneKey) []tree.Key {
	if w.collection != "" && z.Collection != w.collection {
		return out
	}
	path := summaryPath(z.Location)
	parentMine := false
	for depth := 0; depth <= len(path); depth++ {
		key := tree.Key{Collection: z.Collection, Location: summaryLocation(path[:depth])}
		if known, ok := w.seen[key]; ok {
			if known.stop {
				break
			}
			parentMine = known.mine
			continue
		}
		zone, err := tree.NewZoneKey(key.Collection, key.Location)
		if err != nil {
			break
		}
		mine := w.cell.Contains(zone.ID)
		if mine && !parentMine && depth > 0 {
			out = append(out, key)
		}
		stop := !w.prefixes[depth].Contains(zone.ID) || w.prefixes[depth].Mask == w.cell.Mask
		w.seen[key] = boundaryMark{mine, stop}
		if stop {
			break
		}
		parentMine = mine
	}
	return out
}

// boundaryOf is the boundary above a zone this node holds: the
// highest zone of the chain of zones held here that contains it.
func (t *Tree) boundaryOf(collection, location string) (tree.Key, bool) {
	cell, err := tree.Cell(t.View(), t.name)
	if err != nil {
		return tree.Key{}, false
	}
	return boundaryIn(cell, collection, location)
}

func boundaryIn(cell tree.Region, collection, location string) (tree.Key, bool) {
	boundary := ""
	for at := location; at != ""; at = tree.BASE64.Parent(at) {
		z, err := tree.NewZoneKey(collection, at)
		if err != nil {
			return tree.Key{}, false
		}
		if !cell.Contains(z.ID) {
			break
		}
		boundary = at
	}
	if boundary == "" || boundary == tree.BASE64.Root() {
		return tree.Key{}, false
	}
	return tree.Key{Collection: collection, Location: boundary}, true
}

// queueEdgeLocked marks a boundary to push: its value is read from the
// store when the flush sends it.
func (t *Tree) queueEdgeLocked(key tree.Key) {
	if t.Edges == nil {
		t.Edges = map[tree.Key]*Edge{}
	}
	edge := t.Edges[key]
	if edge == nil {
		edge = &Edge{}
		t.Edges[key] = edge
	}
	edge.Stale = true
	if t.pendingEdges == nil {
		t.pendingEdges = map[tree.Key]struct{}{}
	}
	t.pendingEdges[key] = struct{}{}
	_, wait := coalesceDecision(edge.lastAttempt, time.Now(), summaryRemoteWindow)
	if wait == 0 {
		wait = summaryRemoteWindow
	}
	t.armFlushLocked(time.Now().Add(wait))
}

// queueAboveLocked queues the boundary that holds a zone, if any.
func (t *Tree) queueAboveLocked(collection, location string) {
	if key, ok := t.boundaryOf(collection, location); ok {
		t.queueEdgeLocked(key)
	}
}

// wireWithLocked is wireLocked reading zone totals through sums.
func (t *Tree) wireWithLocked(key tree.Key, sums *zoneSums) (mesh.Contribution, error) {
	value, exact, err := t.valueWithLocked(key.Collection, key.Location, sums)
	if err != nil {
		return mesh.Contribution{}, err
	}
	t.revision++
	generation := uint64(0)
	if v := t.View(); v != nil {
		generation = v.Generation
	}
	endpoint, err := t.service.Store.Endpoint(t.service.Self.Host)
	if err != nil {
		return mesh.Contribution{}, err
	}
	return mesh.Contribution{Incomplete: t.exportIncompleteLocked(key.Collection, key.Location), Collection: key.Collection, Location: key.Location, Epoch: t.epoch, Revision: t.revision, Generation: generation, Holder: endpoint, Count: value.Count(), Metrics: append([]float64(nil), value.Metrics()...), Exact: exact, Digest: t.descriptionDigest(key.Collection)}, nil
}

// refreshLocked forgets contributions of children no longer remote or
// no longer current, and queues every boundary: after a bulk, a handoff or
// a change of view.
func (t *Tree) refreshLocked(view *tree.View) {
	t.storeGen++
	for key, value := range t.incoming {
		if !t.OwnsHere(key.Collection, tree.BASE64.Parent(key.Location)) || !t.ChildHolderCurrent(key.Collection, key.Location, value.holder) {
			t.indexIncomingLocked(key, false)
			delete(t.incoming, key)
		}
	}
	keep := map[tree.Key]bool{}
	for _, key := range t.boundaries("", view) {
		keep[key] = true
		t.queueEdgeLocked(key)
	}
	for key := range t.Edges {
		if !keep[key] {
			delete(t.Edges, key)
			delete(t.pendingEdges, key)
		}
	}
}

// summaryPath is a location as a prefix of its descendants: "" for the root.
func summaryPath(location string) string {
	if location == tree.BASE64.Root() {
		return ""
	}
	return location
}

func summaryLocation(prefix string) string {
	if prefix == "" {
		return tree.BASE64.Root()
	}
	return prefix
}

// newerContribution: a new holder, or a later epoch, generation or revision.
func newerContribution(old, in summaryContribution) bool {
	if old.holder != in.holder {
		return true
	}
	if in.epoch != old.epoch {
		return in.epoch > old.epoch
	}
	if in.generation != old.generation {
		return in.generation > old.generation
	}
	return in.revision > old.revision
}
