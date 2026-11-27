package summary

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestPackedBoundariesMatchNativeScopesAndCursors(t *testing.T) {
	var keys []tree.Key
	for _, col := range []string{"Large", "a", "z"} {
		for _, loc := range []string{"-", "-0", "0", "@", "A", "A0", "_", "a", "zzzzzzzzzzzzzzzz"} {
			keys = append(keys, tree.Key{Collection: col, Location: loc})
		}
	}
	cursors := append(slices.Clone(keys), tree.Key{}, tree.Key{Collection: "M"}, tree.Key{Collection: "a", Location: "B"})
	cache := &exportKeys{packed: packBoundaries(keys, 2048)}
	if len(cache.packed) == 0 {
		t.Fatal("missing compact inventory")
	}
	for _, col := range []string{"", "Large", "a", "z", "absent"} {
		for _, cursor := range cursors {
			var want []tree.Key
			for _, key := range keys {
				if (col == "" || key.Collection == col) && key.Compare(cursor) > 0 {
					want = append(want, key)
				}
			}
			var got []tree.Key
			keys := cache.after(col, cursor)
			for key, ok := keys.next(); ok; key, ok = keys.next() {
				got = append(got, key)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("collection=%q after %+v returned %+v, want %+v", col, cursor, got, want)
			}
		}
	}
}

func TestPackedInventoryPreservesPublishedPagesAfterExtension(t *testing.T) {
	const collection = "Many000000000000"
	tr := deepBoundaryTree(t, 256)
	tr.ExportCacheBytes = 8192
	keys, fold := tr.exportKeysFor(collection, false)
	if len(keys) != 256 || fold == nil || tr.exportKeys == nil || len(tr.exportKeys.packed) == 0 || tr.exportKeys.keys != nil {
		t.Fatal("fixture must retain a compact inventory")
	}
	original := tr.exportKeys
	page := &mesh.SummaryPage{Collection: collection, Location: keys[251].Location, Limit: 3}
	want, more := tr.ExportPage(collection, nil, "parent", page)
	if len(want) != 3 || !more {
		t.Fatal("fixture must have a fourth contribution after this cursor")
	}
	loc := fmt.Sprintf("%015x1", 256)
	if _, err := tr.service.Store.Write("later", storage.Record{Collection: collection, Location: loc, ID: "later"}); err != nil {
		t.Fatal(err)
	}
	got, stillMore := tr.ExportPage(collection, nil, "parent", page)
	if !stillMore || len(got) != len(want) {
		t.Fatalf("continuing page has %d contributions, more=%v", len(got), stillMore)
	}
	for i, previous := range want {
		// Every export is freshly versioned, even with identical values.
		previous.Revision = got[i].Revision
		if !reflect.DeepEqual(previous, got[i]) {
			t.Fatalf("contribution changed before fresh discovery: %+v vs %+v", previous, got[i])
		}
	}
	fresh, _ := tr.exportKeysFor(collection, false)
	if len(fresh) != 257 || fresh[256].Location != loc || !slices.Equal(original.forCollection(collection), keys) {
		t.Fatal("extending a compact inventory broke its published snapshot or hid a new boundary")
	}
	if tr.stats.exportKeysOversized.Load() != 0 {
		t.Fatal("a compact inventory that fit was counted as dropped")
	}
	shorter := deepBoundaryTree(t, 32)
	inventory, err := shorter.service.Store.ZoneInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.extendExportKeys(tr.exportKeys, inventory); ok {
		t.Fatal("a removed zone must invalidate incremental extension")
	}
	if _, err := tr.service.Store.Write("delete", storage.Record{Collection: collection, Location: loc, ID: "later", Deleted: true}); err != nil {
		t.Fatal(err)
	}
	repair, _ := tr.ExportPage(collection, []string{loc}, "missing", nil)
	if len(repair) != 1 || repair[0].Count != 0 {
		t.Fatal("an explicit repair must observe the deleted value")
	}
	// Force a full discovery under a budget too small even for a compact index.
	tr.ExportCacheBytes = 128
	tr.exportKeys = nil
	uncached, _ := tr.exportKeysFor(collection, false)
	if tr.exportKeys != nil || !slices.Equal(uncached, fresh) || tr.stats.exportKeysOversized.Load() != 1 {
		t.Fatal("oversized inventory must answer the current poll without remaining in the cache")
	}
}

func TestPackedCacheBudgetAtSixteenByteDepth(t *testing.T) {
	keys := make([]tree.Key, 65536)
	for i := range keys {
		keys[i] = tree.Key{Collection: "Many000000000000", Location: fmt.Sprintf("%016x", i)}
	}
	// 60,000 rows of 17 bytes plus a collection fit. 65,536 do not.
	if packBoundaries(keys[:60000], 1<<20) == nil || packBoundaries(keys, 1<<20) != nil {
		t.Fatal("deep-boundary budget not enforced")
	}
}
