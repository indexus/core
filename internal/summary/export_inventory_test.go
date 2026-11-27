package summary

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func (t *Tree) exportKeysFor(collection string, continuing bool) ([]tree.Key, *foldMark) {
	inventory, fold := t.exportInventory(continuing)
	return inventory.forCollection(collection), fold
}

func (t *Tree) boundaryKeys(collection string, view *tree.View, continuing bool) ([]tree.Key, error) {
	inventory, err := t.boundaryInventory(view, continuing)
	return inventory.forCollection(collection), err
}

func inventoryTree(t testing.TB) *Tree {
	s := describedService(t, "export-cache", peerTransport{})
	initCluster(t, s)
	collection := "Demo000000000000"
	leaf, _ := tree.NewZoneKey(collection, "1000000000000000")
	root, _ := tree.NewZoneKey(collection, "@")
	view, err := tree.NewViewFrom(1, []tree.VNodeDescriptor{{Name: s.Self.VNode, ID: leaf.ID}, {Name: "parent", ID: root.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		_, err := s.Store.Write(fmt.Sprint(i), storage.Record{Collection: collection, Location: fmt.Sprintf("1%015d", i), ID: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	tr := New(s, 1, Hooks{})
	tr.SetView(view, true, false)
	tr.Summaries = &foldMark{digest: view.Digest}
	return tr
}

func TestExportInventorySharesDiscoveryAndInvalidatesCommittedChanges(t *testing.T) {
	tr := inventoryTree(t)
	first, fold := tr.exportKeysFor("", false)
	if len(first) == 0 || fold == nil || tr.exportKeys == nil {
		t.Fatal("vacuous boundary inventory")
	}
	cached := tr.exportKeys
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			keys, got := tr.exportKeysFor("", false)
			if got != fold || !reflect.DeepEqual(keys, first) {
				t.Error("concurrent reader changed inventory")
			}
		})
	}
	workers.Wait()
	if tr.exportKeys != cached {
		t.Fatal("unchanged polls rebuilt discovery")
	}
	// A write can add an unseen collection/zone without a new topology or fold.
	if _, err := tr.service.Store.Write("new", storage.Record{Collection: "Other00000000000", Location: "1000000000000000", ID: "new"}); err != nil {
		t.Fatal(err)
	}
	tr.exportKeysFor("", false)
	if tr.exportKeys == cached || tr.exportKeys.revision == cached.revision {
		t.Fatal("new committed inventory stayed hidden")
	}
	cached = tr.exportKeys
	tr.Mu.Lock()
	tr.Summaries = &foldMark{digest: tr.View().Digest}
	tr.Mu.Unlock()
	_, newFold := tr.exportKeysFor("", false)
	if tr.exportKeys != cached || newFold == fold {
		t.Fatal("an unchanged spatial inventory was not reused for the fresh fold")
	}
	cell, _ := tree.Cell(tr.View(), tr.name)
	view := tr.View().Clone()
	remote, _ := view.IDOf("parent")
	remote[11] ^= 1
	if err := view.Add("unrelated", remote); err != nil {
		t.Fatal(err)
	}
	after, _ := tree.Cell(view, tr.name)
	if after != cell {
		t.Fatal("fixture did not preserve the local cell")
	}
	tr.SetView(view, true, false)
	tr.Mu.Lock()
	tr.Summaries = &foldMark{digest: view.Digest}
	tr.Mu.Unlock()
	tr.exportKeysFor("", false)
	if tr.exportKeys != cached {
		t.Fatal("an unrelated membership change rebuilt local boundaries")
	}
	mine, _ := view.IDOf(tr.name)
	mine[11] ^= 1
	view = view.Clone()
	if err := view.Add("competing", mine); err != nil {
		t.Fatal(err)
	}
	after, _ = tree.Cell(view, tr.name)
	if after == cell {
		t.Fatal("fixture did not split the local cell")
	}
	tr.SetView(view, true, false)
	tr.Mu.Lock()
	tr.Summaries = &foldMark{digest: view.Digest}
	tr.Mu.Unlock()
	tr.exportKeysFor("", false)
	if tr.exportKeys == cached {
		t.Fatal("a changed local cell reused old boundary discovery")
	}
	// A bounded cache never retains an oversized list; discovery still returns it.
	tr.ExportCacheBytes = 1
	tr.exportMu.Lock()
	tr.exportKeys = nil
	tr.exportMu.Unlock()
	keys, _ := tr.exportKeysFor("", false)
	if len(keys) == 0 || tr.exportKeys != nil {
		t.Fatal("oversized inventory retained or dropped")
	}
}

func TestExportInventoryFiltersScopesAfterSharedDiscoveryAndKeepsExplicitRepair(t *testing.T) {
	tr := inventoryTree(t)
	all, _ := tr.ExportPage("", nil, "", nil)
	if len(all) == 0 {
		t.Fatal("vacuous export")
	}
	for _, parent := range []string{"parent", tr.name, "missing"} {
		for _, collection := range []string{"", "Demo000000000000", "absent"} {
			got, _ := tr.ExportPage(collection, nil, parent, nil)
			var want []mesh.Contribution
			for _, c := range all {
				key, _ := tree.NewZoneKey(c.Collection, tree.BASE64.Parent(c.Location))
				owner, _ := tr.View().OwnerName(key.ID)
				if (collection == "" || collection == c.Collection) && (parent == tr.name || parent == owner) {
					want = append(want, c)
				}
			}
			if len(got) != len(want) {
				t.Fatalf("parent=%s collection=%s got=%d want=%d", parent, collection, len(got), len(want))
			}
		}
	}
	c := all[0]
	got, _ := tr.ExportPage(c.Collection, []string{c.Location}, "missing", nil)
	if len(got) != 1 {
		t.Fatal("explicit point repair was filtered as a parent poll")
	}
}

func BenchmarkSharedExportInventory(b *testing.B) {
	for _, retained := range []bool{false, true} {
		b.Run(fmt.Sprintf("retained=%v", retained), func(b *testing.B) {
			tr := inventoryTree(b)
			if !retained {
				tr.ExportCacheBytes = 0
			}
			tr.exportKeysFor("", false)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				tr.exportKeysFor("", false)
			}
		})
	}
}

func TestInventoryDiscoveryDoesNotBlockLocalWriteNotification(t *testing.T) {
	tr := inventoryTree(t)
	// This lock-only check exercises notification, not the asynchronous flush.
	tr.pushMu.Lock()
	tr.pushing = true
	tr.pushMu.Unlock()
	t.Cleanup(func() {
		tr.dirtyMu.Lock()
		if tr.dirtyTimer != nil {
			tr.dirtyTimer.Stop()
			tr.dirtyTimer = nil
		}
		tr.dirtyMu.Unlock()
	})
	tr.exportMu.Lock()
	done := make(chan struct{})
	go func() {
		cell, _ := tree.Cell(tr.View(), tr.name)
		tr.OnLocalWrite(storage.Record{Collection: "Demo000000000000", Location: "1000000000000000", ID: "new"}, nil, cell)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		tr.exportMu.Unlock()
		t.Fatal("write notification waited for inventory discovery")
	}
	tr.exportMu.Unlock()
	tr.dirtyMu.Lock()
	marked := len(tr.dirty) > 0
	tr.dirtyMu.Unlock()
	if !marked {
		t.Fatal("vacuous callback: no boundary was marked")
	}
}

func TestCachedInventoryDoesNotCacheValuesOrHideTombstoneResurrection(t *testing.T) {
	tr := inventoryTree(t)
	before, _ := tr.ExportPage("", nil, "", nil)
	if len(before) == 0 {
		t.Fatal("vacuous positive contribution")
	}
	cached := tr.exportKeys
	for i := range 8 {
		_, err := tr.service.Store.Write(fmt.Sprint("delete", i), storage.Record{Collection: "Demo000000000000", Location: fmt.Sprintf("1%015d", i), ID: fmt.Sprint(i), Deleted: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	zero, _ := tr.ExportPage("", nil, "", nil)
	for _, c := range zero {
		if c.Count != 0 {
			t.Fatal("cached key resurrected a deleted value")
		}
	}
	if tr.exportKeys != cached {
		t.Fatal("unchanged canonical identities rebuilt the catalog")
	}
	key := before[0]
	repair, _ := tr.ExportPage(key.Collection, []string{key.Location}, "missing", nil)
	if len(repair) != 1 || repair[0].Count != 0 {
		t.Fatal("explicit zero repair was hidden")
	}
	_, err := tr.service.Store.Write("resurrect", storage.Record{Collection: "Demo000000000000", Location: "1000000000000000", ID: "0"})
	if err != nil {
		t.Fatal(err)
	}
	live, _ := tr.ExportPage("", nil, "", nil)
	if len(live) == 0 || tr.exportKeys != cached {
		t.Fatal("tombstone resurrection was not visible through the same inventory")
	}
}

// A poll is paged by cursor over one list of boundaries. AWS 2026-11-26: a
// donor that creates zones changes its revision at every write, and each
// page of each poll rebuilt the inventory (38 % of its CPU). A later page
// reads the list its poll started on; the next poll sees the new zone.
func TestLaterPagesOfAPollReadTheInventoryItStartedOn(t *testing.T) {
	tr := inventoryTree(t)
	first, _ := tr.exportKeysFor("", false)
	built := tr.stats.exportKeysBuilt.Load()
	if _, err := tr.service.Store.Write("new", storage.Record{Collection: "Other00000000000", Location: "1000000000000000", ID: "new"}); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if page, _ := tr.exportKeysFor("", true); !reflect.DeepEqual(page, first) {
			t.Fatal("a later page of the poll changed its list")
		}
	}
	if got := tr.stats.exportKeysBuilt.Load() - built; got != 0 {
		t.Fatalf("8 later pages built %d inventories", got)
	}
	next, _ := tr.exportKeysFor("", false)
	if len(next) <= len(first) {
		t.Fatalf("the next poll holds %d boundaries after %d", len(next), len(first))
	}
}

// Zones created since the last poll extend its boundaries at their own
// cost: the list equals the one built from the whole inventory, and nothing
// is built again. A removed zone starts over.
func TestBoundariesFollowAddedZonesWithoutARebuild(t *testing.T) {
	tr := inventoryTree(t)
	tr.exportKeysFor("", false)
	built := tr.stats.exportKeysBuilt.Load()
	cell, _ := tree.Cell(tr.View(), tr.name)
	random := rand.New(rand.NewSource(1))
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	total := 0
	for round := range 20 {
		for range 1 + random.Intn(40) {
			location := []byte("1000000000000000")
			for i := random.Intn(4); i < 16; i++ { // inside and outside the cell, at every depth
				location[i] = alphabet[random.Intn(64)]
			}
			collection := []string{"Demo000000000000", "Other00000000000"}[random.Intn(2)]
			total++
			if _, err := tr.service.Store.Write(fmt.Sprint("w", total), storage.Record{Collection: collection, Location: string(location), ID: fmt.Sprint(total)}); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := tr.exportKeysFor("", false)
		inventory, err := tr.service.Store.ZoneInventory(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := boundariesIn("", cell, inventory)
		sort.Slice(want, func(i, j int) bool { return want[i].Compare(want[j]) < 0 })
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: %d boundaries followed, %d built from the inventory", round, len(got), len(want))
		}
	}
	if got := tr.stats.exportKeysBuilt.Load() - built; got != 0 {
		t.Fatalf("%d zones added in 20 polls built %d inventories", total, got)
	}
	if len(tr.exportKeys.keys) < 20 {
		t.Fatalf("vacuous: %d boundaries", len(tr.exportKeys.keys))
	}
}

// One inventory serves the polls of every collection, each with its own
// boundaries only (measured 2026-11-26: polls alternating between two
// collections rebuilt it every time).
func TestPollsOfDifferentCollectionsShareOneInventory(t *testing.T) {
	tr := inventoryTree(t)
	if _, err := tr.service.Store.Write("other", storage.Record{Collection: "Other00000000000", Location: "1000000000000000", ID: "other"}); err != nil {
		t.Fatal(err)
	}
	all, _ := tr.exportKeysFor("", false)
	built := tr.stats.exportKeysBuilt.Load()
	seen := 0
	for i := range 8 {
		collection := []string{"Demo000000000000", "Other00000000000"}[i%2]
		keys, _ := tr.exportKeysFor(collection, false)
		if len(keys) == 0 {
			t.Fatalf("no boundary of %s", collection)
		}
		for _, key := range keys {
			if key.Collection != collection {
				t.Fatalf("a poll of %s got a boundary of %s", collection, key.Collection)
			}
		}
		if i < 2 {
			seen += len(keys)
		}
	}
	if seen != len(all) {
		t.Fatalf("the two collections hold %d boundaries, the inventory %d", seen, len(all))
	}
	if got := tr.stats.exportKeysBuilt.Load() - built; got != 0 {
		t.Fatalf("8 polls alternating between two collections built %d inventories", got)
	}
}

// Time a late parent poll on a retained inventory. Its cursor has already
// passed a large prefix. Keep the live page identical for both measurements.
func BenchmarkLateParentExportPage(b *testing.B) {
	for _, prefix := range []int{0, 8192} {
		b.Run(fmt.Sprintf("prefix=%d", prefix), func(b *testing.B) {
			tr := inventoryTree(b)
			live, _ := tr.exportKeysFor("", false)
			var keys []tree.Key
			for i := range prefix {
				keys = append(keys, tree.Key{Collection: "Bulk000000000000", Location: fmt.Sprintf("%016d", i)})
			}
			tr.exportKeys.keys = append(keys, live...)
			page := &mesh.SummaryPage{Collection: "Bulk000000000000", Location: "zzzzzzzzzzzzzzzz", Limit: 1}
			if got, _ := tr.ExportPage("", nil, "parent", page); len(got) != 1 {
				b.Fatal("no live boundary on the late page")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				tr.ExportPage("", nil, "parent", page)
			}
		})
	}
}
