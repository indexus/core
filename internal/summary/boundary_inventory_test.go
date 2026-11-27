package summary

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// The oracle enumerates every record's ancestors without pruning, child sums,
// inventory caching, or cell-membership predicates. This also covers records at non-leaf zones.
func boundaryOracle(records []storage.Record, view *tree.View, name, collection string) []tree.Key {
	set := map[tree.Key]bool{}
	for _, r := range records {
		if r.Deleted || collection != "" && collection != r.Collection {
			continue
		}
		for at := r.Location; at != "" && at != "@"; at = tree.BASE64.Parent(at) {
			child, _ := tree.NewZoneKey(r.Collection, at)
			parent, _ := tree.NewZoneKey(r.Collection, tree.BASE64.Parent(at))
			owner, _ := view.OwnerName(child.ID)
			above, _ := view.OwnerName(parent.ID)
			if owner == name && above != name {
				set[tree.Key{Collection: r.Collection, Location: at}] = true
			}
		}
	}
	var keys []tree.Key
	for key := range set {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Compare(keys[j]) < 0 })
	return keys
}

func TestBoundaryInventoryMatchesAncestorOracleWithoutSubtreeSums(t *testing.T) {
	s := describedService(t, "inventory-oracle", peerTransport{})
	initCluster(t, s)
	rng := rand.New(rand.NewSource(732))
	var records []storage.Record
	for i := range 256 {
		path := make([]byte, 1+i%16)
		for j := range path {
			path[j] = routingAlphabet[rng.Intn(64)]
		}
		records = append(records, storage.Record{Collection: fmt.Sprintf("Lab%013d", i%8), Location: string(path), ID: fmt.Sprint(i)})
	}
	if err := s.Store.SeedLocalRecords(records); err != nil {
		t.Fatal(err)
	}
	inventory, err := s.Store.ZoneInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tr := New(s, 1, Hooks{})
	for _, n := range []int{1, 4, 16, 64} {
		for _, clustered := range []bool{false, true} {
			active := []tree.VNodeDescriptor{s.VNode}
			for i := 1; i < n; i++ {
				var id tree.ID
				if !clustered {
					rng.Read(id[:])
				}
				id[10], id[11] = byte(i>>8), byte(i+2)
				active = append(active, tree.VNodeDescriptor{Name: fmt.Sprint("peer", i), ID: id})
			}
			view, err := tree.NewViewFrom(1, active)
			if err != nil {
				t.Fatal(err)
			}
			for _, collection := range []string{"", "Lab0000000000000", "absent"} {
				cell, err := tree.Cell(view, tr.name)
				if err != nil {
					t.Fatal(err)
				}
				got := boundariesIn(collection, cell, inventory)
				sort.Slice(got, func(i, j int) bool { return got[i].Compare(got[j]) < 0 })
				want := boundaryOracle(records, view, tr.name, collection)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("n=%d clustered=%v collection=%s: got %v want %v", n, clustered, collection, got, want)
				}
			}
		}
	}
}
