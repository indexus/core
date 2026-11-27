package summary

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// A stable catalog is not a write workload creating fresh zones. Measure
// cache rebuilds separately from total query time; no timing assertion is a
// performance guarantee on a developer's machine.
func TestInventoryDiscoveryWorkUnderChurn(t *testing.T) {
	for _, zones := range []int{1024, 8192} {
		for _, mode := range []string{"stable", "new-zone", "alternating-collections", "oversized"} {
			t.Run(fmt.Sprintf("zones=%d/%s", zones, mode), func(t *testing.T) {
				service := describedService(t, "inventory-cost", peerTransport{})
				initCluster(t, service)
				leaf, _ := tree.NewZoneKey("Demo000000000000", "1000000000000000")
				root, _ := tree.NewZoneKey("Demo000000000000", "@")
				view, err := tree.NewViewFrom(1, []tree.VNodeDescriptor{{Name: service.Self.VNode, ID: leaf.ID}, {Name: "parent", ID: root.ID}})
				if err != nil {
					t.Fatal(err)
				}
				tr := New(service, 1, Hooks{})
				tr.SetView(view, true, false)
				var records []storage.Record
				seed := make([]storage.Record, zones)
				for i := range seed {
					collection := []string{"Demo000000000000", "Other00000000000"}[i%2]
					seed[i] = storage.Record{Collection: collection, Location: fmt.Sprintf("1%015x", i), ID: fmt.Sprint("seed-", i)}
				}
				if err := tr.service.Store.SeedLocalRecords(seed); err != nil {
					t.Fatal(err)
				}
				records = append(records, seed...)
				if mode == "oversized" {
					tr.ExportCacheBytes = 1
				}
				var elapsed time.Duration
				var rebuilds, extensions int64
				for i := range 8 {
					collection := ""
					if mode == "alternating-collections" {
						collection = []string{"Demo000000000000", "Other00000000000"}[i%2]
					}
					if mode == "new-zone" {
						r := storage.Record{Collection: "Demo000000000000", Location: fmt.Sprintf("2%015x", i), ID: fmt.Sprint("new-", i)}
						if _, err := tr.service.Store.Write(r.ID, r); err != nil {
							t.Fatal(err)
						}
						records = append(records, r)
					}
					builtBefore := tr.stats.exportKeysBuilt.Load()
					extendedBefore := tr.stats.exportKeysExtended.Load()
					started := time.Now()
					got, err := tr.boundaryKeys(collection, tr.View(), false)
					elapsed += time.Since(started)
					if err != nil {
						t.Fatal(err)
					}
					rebuilds += tr.stats.exportKeysBuilt.Load() - builtBefore
					extensions += tr.stats.exportKeysExtended.Load() - extendedBefore
					if i == 7 && !reflect.DeepEqual(got, boundaryOracle(records, tr.View(), tr.name, collection)) {
						t.Fatal("boundary export differs from the independent ancestor oracle")
					}
				}
				inventory, err := tr.service.Store.ZoneInventory(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("seed_zones=%d mode=%s polls=8 rebuilds=%d extensions=%d revision=%d discovery_time=%s", zones, mode, rebuilds, extensions, inventory.Revision(), elapsed)
			})
		}
	}
}
