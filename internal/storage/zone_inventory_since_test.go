package storage

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func sinceZone(t *testing.T, i int) tree.ZoneKey {
	t.Helper()
	z, err := tree.NewZoneKey("Demo000000000000", fmt.Sprintf("z%015d", i))
	if err != nil {
		t.Fatal(err)
	}
	return z
}

// Since returns exactly the zones added, whatever the catalog holds, and
// refuses an inventory that lost a zone.
func TestInventorySinceIsTheZonesAdded(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	var root *spatialNode
	next := 0
	for round := range 200 {
		old := ZoneInventory{root: root}
		var want []tree.ZoneKey
		for range random.Intn(6) {
			z := sinceZone(t, next)
			next++
			root = spatialSet(root, z, true)
			want = append(want, z)
		}
		var got []tree.ZoneKey
		if !(ZoneInventory{root: root}).Since(old, func(z tree.ZoneKey) { got = append(got, z) }) {
			t.Fatalf("round %d: additions refused", round)
		}
		sort.Slice(want, func(i, j int) bool { return spatialCompare(want[i], want[j]) < 0 })
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("round %d: since=%v, added=%v", round, got, want)
		}
		if next == 0 || round%10 != 9 {
			continue
		}
		before := ZoneInventory{root: root}
		root = spatialSet(root, sinceZone(t, random.Intn(next)), false)
		if spatialCount(root) < spatialCount(before.root) && (ZoneInventory{root: root}).Since(before, func(tree.ZoneKey) {}) {
			t.Fatalf("round %d: a removed zone was not reported", round)
		}
		next += 1000 // removed names are not added again
	}
}

// The walk costs the zones added and their paths, not the catalog: commits
// share every subtree they did not touch.
func TestInventorySinceSkipsWhatACommitDidNotTouch(t *testing.T) {
	var root *spatialNode
	for i := range 40000 {
		root = spatialSet(root, sinceZone(t, i), true)
	}
	old := ZoneInventory{root: root}
	for i := range 8 {
		root = spatialSet(root, sinceZone(t, 100000+i), true)
	}
	before := sinceExpands.Load()
	added := 0
	if !(ZoneInventory{root: root}).Since(old, func(tree.ZoneKey) { added++ }) || added != 8 {
		t.Fatalf("added=%d, want 8", added)
	}
	if expands := sinceExpands.Load() - before; expands > 200 {
		t.Fatalf("8 zones added to 40000: %d subtrees opened", expands)
	} else {
		t.Logf("8 zones added to 40000: %d subtrees opened", expands)
	}
}
