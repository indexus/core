package summary

import (
	"context"
	"slices"
	"sort"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// One immutable boundary inventory is shared by all parent polls. The key
// depends on topology and the spatial catalog, never metadata-only commits.
// Contributions are always read anew and rechecked for local authority.
type exportKeys struct {
	cell      tree.Region
	revision  uint64
	keys      []tree.Key
	packed    []packedGroup
	inventory storage.ZoneInventory
}

// A page after the first continues a poll: it reads the inventory its poll
// started on, when that one is still held.
func (t *Tree) exportInventory(continuing bool) (*exportKeys, *foldMark) {
	t.Mu.Lock()
	fold := t.Summaries
	t.Mu.Unlock()
	if fold == nil {
		return nil, nil
	}
	keys, err := t.boundaryInventory(t.View(), continuing)
	if err != nil {
		return nil, nil
	}
	return keys, fold
}

// Ownership transitions depend on this vnode's cell, not on every remote
// membership change. Parent destinations are filtered against the current view.
func (t *Tree) boundaryInventory(view *tree.View, continuing bool) (*exportKeys, error) {
	cell, err := tree.Cell(view, t.name)
	if err != nil {
		return nil, err
	}
	// Only inventory readers wait here. Local writes and summary acceptance do
	// not take this mutex; concurrent polls coalesce into one discovery.
	t.exportMu.Lock()
	defer t.exportMu.Unlock()
	cached := t.exportKeys
	fits := cached != nil && cached.cell == cell
	if fits && continuing {
		// A node that creates zones changes its revision at every write:
		// each page of a poll then rebuilt the whole inventory (AWS
		// 2026-11-26: 38 % of a donor's CPU in boundaryKeys). The cursor of
		// a poll walks the list its first page built; a boundary created
		// since is in the next poll's.
		t.stats.exportKeysContinued.Add(1)
		return cached, nil
	}
	inventory, err := t.service.Store.ZoneInventory(context.Background())
	if err != nil {
		return nil, err
	}
	if fits && cached.revision == inventory.Revision() {
		t.stats.exportKeysReused.Add(1)
		return cached, nil
	}
	if fits {
		if keys, ok := t.extendExportKeys(cached, inventory); ok {
			return keys, nil
		}
	}
	t.stats.exportKeysBuilt.Add(1)
	// One inventory of every collection serves the polls of each: an
	// inventory of one did not serve another, and polls alternating between
	// two collections rebuilt it every time (measured 2026-11-26, 45eb506).
	keys := boundariesIn("", cell, inventory)
	sort.Slice(keys, func(i, j int) bool { return keys[i].Compare(keys[j]) < 0 })
	return t.retainExportKeys(&exportKeys{cell: cell, revision: inventory.Revision(), keys: keys, inventory: inventory}), nil
}

// ofCollection is the part of the sorted boundaries that belongs to
// collection, all of them for "": a range of the list, not a copy.
func ofCollection(keys []tree.Key, collection string) []tree.Key {
	if collection == "" {
		return keys
	}
	from := sort.Search(len(keys), func(i int) bool { return keys[i].Collection >= collection })
	to := from + sort.Search(len(keys)-from, func(i int) bool { return keys[from+i].Collection > collection })
	return keys[from:to:to]
}

// extendExportKeys brings cached to inventory when zones were only added:
// the boundaries of the new zones join the list, at the cost of those zones.
// A node under load creates zones at every write, and each first page of a
// poll rebuilt the boundaries of all of them (AWS 2026-11-26: 38 % of a
// donor's CPU). Neither the list nor its record is changed in place: polls read them unlocked.
func (t *Tree) extendExportKeys(cached *exportKeys, inventory storage.ZoneInventory) (*exportKeys, bool) {
	walk := newBoundaryWalk("", cached.cell)
	var found []tree.Key
	if !inventory.Since(cached.inventory, func(z tree.ZoneKey) { found = walk.add(found, z) }) {
		return nil, false
	}
	t.stats.exportKeysExtended.Add(1)
	next := *cached
	next.revision, next.inventory = inventory.Revision(), inventory
	if len(cached.packed) > 0 {
		// Most additions under an existing boundary leave its identity unchanged.
		found = slices.DeleteFunc(found, cached.packedContains)
	}
	if len(found) == 0 {
		t.exportKeys = &next
		return &next, true
	}
	next.keys = mergeBoundaryKeys(cached.forCollection(""), found)
	next.packed = nil
	return t.retainExportKeys(&next), true
}

// mergeBoundaryKeys publishes one immutable union. Cloning the old list
// separately for every new boundary costs additions * cached boundaries.
func mergeBoundaryKeys(previous, added []tree.Key) []tree.Key {
	slices.SortFunc(added, tree.Key.Compare)
	added = slices.CompactFunc(added, func(a, b tree.Key) bool { return a == b })
	fresh := added[:0]
	for _, key := range added {
		if _, held := slices.BinarySearchFunc(previous, key, tree.Key.Compare); !held {
			fresh = append(fresh, key)
		}
	}
	if len(fresh) == 0 {
		return previous
	}
	keys := make([]tree.Key, 0, len(previous)+len(fresh))
	i, j := 0, 0
	for i < len(previous) && j < len(fresh) {
		if previous[i].Compare(fresh[j]) < 0 {
			keys = append(keys, previous[i])
			i++
		} else {
			keys = append(keys, fresh[j])
			j++
		}
	}
	keys = append(keys, previous[i:]...)
	return append(keys, fresh[j:]...)
}

// A poll always gets a complete inventory. Keep it in its native form if
// it fits, then try a compact copy. Otherwise this poll owns the only copy.
func (t *Tree) retainExportKeys(e *exportKeys) *exportKeys {
	keys := e.keys
	bytes := 128 + cap(keys)*64
	for _, key := range keys {
		bytes += len(key.Collection) + len(key.Location)
	}
	if bytes > t.ExportCacheBytes {
		e.packed = packBoundaries(keys, t.ExportCacheBytes)
		if len(e.packed) == 0 {
			t.stats.exportKeysOversized.Add(1)
			t.exportKeys = nil
			return e
		}
		e.keys = nil
	}
	t.exportKeys = e
	return e
}
