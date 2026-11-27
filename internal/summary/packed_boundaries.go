package summary

import (
	"slices"
	"sort"
	"strings"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// A group owns one collection name and a slab of fixed-width locations.
// Byte zero holds the exact string length. Padding never participates in
// ordering. A page can binary-search or read any location without decoding
// its predecessors or allocating a string per key.
type packedGroup struct {
	collection string
	slab       string
	stride     int
	count      int
}

func (g packedGroup) location(i int) string {
	start := i * g.stride
	return g.slab[start+1 : start+1+int(g.slab[start])]
}

func packBoundaries(keys []tree.Key, budget int) []packedGroup {
	// 128 bytes for the cache header, 64 per reserved group descriptor
	// (48 on 64-bit Go). Only the single arena and descriptors are retained.
	const header, descriptor = 128, 64
	var groups []packedGroup
	used := 0
	for _, key := range keys {
		if len(key.Location) > 16 { // Indexus addresses are at most 16 bytes.
			return nil
		}
		if len(groups) == 0 || groups[len(groups)-1].collection != key.Collection {
			groups = append(groups, packedGroup{collection: key.Collection})
			used += len(key.Collection)
		}
		g := &groups[len(groups)-1]
		stride := max(g.stride, len(key.Location)+1)
		used += stride + (stride-g.stride)*g.count
		g.stride, g.count = stride, g.count+1
		if header+descriptor*cap(groups)+used > budget {
			return nil
		}
	}
	var arena strings.Builder
	arena.Grow(used)
	if header+descriptor*cap(groups)+arena.Cap() > budget {
		return nil
	}
	const padding = "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"
	for i := range groups {
		g := &groups[i]
		start := arena.Len()
		arena.WriteString(g.collection)
		g.collection = arena.String()[start:]
		start = arena.Len()
		for _, key := range keys[:g.count] {
			arena.WriteByte(byte(len(key.Location)))
			arena.WriteString(key.Location)
			arena.WriteString(padding[:g.stride-1-len(key.Location)])
		}
		keys = keys[g.count:]
		g.slab = arena.String()[start:]
	}
	return groups
}

func (e *exportKeys) packedContains(key tree.Key) bool {
	i, found := slices.BinarySearchFunc(e.packed, key.Collection, func(g packedGroup, name string) int {
		return strings.Compare(g.collection, name)
	})
	if !found {
		return false
	}
	g := e.packed[i]
	j := sort.Search(g.count, func(i int) bool { return g.location(i) >= key.Location })
	return j < g.count && g.location(j) == key.Location
}

// boundaryRange traverses a borrowed snapshot starting strictly after the
// lexical cursor. Traversal allocates nothing for either representation.
type boundaryRange struct {
	keys   []tree.Key
	groups []packedGroup
	row    int
}

func (r *boundaryRange) next() (tree.Key, bool) {
	if len(r.keys) > 0 {
		key := r.keys[0]
		r.keys = r.keys[1:]
		return key, true
	}
	for len(r.groups) > 0 {
		g := &r.groups[0]
		if r.row < g.count {
			key := tree.Key{Collection: g.collection, Location: g.location(r.row)}
			r.row++
			return key, true
		}
		r.groups, r.row = r.groups[1:], 0
	}
	return tree.Key{}, false
}

func (e *exportKeys) after(collection string, cursor tree.Key) boundaryRange {
	if e == nil {
		return boundaryRange{}
	}
	if len(e.packed) == 0 {
		return boundaryRange{keys: afterCursor(ofCollection(e.keys, collection), cursor)}
	}
	first := max(collection, cursor.Collection)
	i := sort.Search(len(e.packed), func(i int) bool { return e.packed[i].collection >= first })
	groups := e.packed[i:]
	if len(groups) == 0 || collection != "" && groups[0].collection != collection {
		return boundaryRange{}
	}
	if collection != "" {
		groups = groups[:1]
	}
	from := 0
	if g := &groups[0]; g.collection == cursor.Collection {
		from = sort.Search(g.count, func(i int) bool { return g.location(i) > cursor.Location })
	}
	return boundaryRange{groups: groups, row: from}
}

func afterCursor(keys []tree.Key, cursor tree.Key) []tree.Key {
	if cursor == (tree.Key{}) {
		return keys
	}
	i := sort.Search(len(keys), func(i int) bool { return cursor.Compare(keys[i]) < 0 })
	return keys[i:]
}

// Only callers which require a slice materialize a compact inventory.
func (e *exportKeys) forCollection(collection string) []tree.Key {
	if e == nil {
		return nil
	}
	if len(e.packed) == 0 {
		return ofCollection(e.keys, collection)
	}
	r := e.after(collection, tree.Key{})
	n := 0
	for _, g := range r.groups {
		n += g.count
	}
	keys := make([]tree.Key, 0, n)
	// Materialized keys may remain in the edge map or in a later native
	// inventory. Give them independent strings instead of pinning this arena.
	for _, g := range r.groups {
		name := strings.Clone(g.collection)
		for i := 0; i < g.count; i++ {
			keys = append(keys, tree.Key{Collection: name, Location: strings.Clone(g.location(i))})
		}
	}
	return keys
}
