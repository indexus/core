package storage

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// The zone index knows how many zones each subtree holds, through every
// insertion, removal and rotation: the zone of a rank is found without
// walking the zones before it.
func TestSpatialIndexKnowsTheRankOfEveryZone(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	var root *spatialNode
	present := map[tree.ZoneKey]bool{}
	for step := range 4000 {
		z, err := tree.NewZoneKey(fmt.Sprintf("collection-%05d", rng.Intn(4)), fmt.Sprintf("%c%c%c", 'a'+rng.Intn(6), 'a'+rng.Intn(6), 'a'+rng.Intn(6)))
		if err != nil {
			t.Fatal(err)
		}
		add := rng.Intn(3) > 0
		root = spatialSet(root, z, add)
		if add {
			present[z] = true
		} else {
			delete(present, z)
		}
		if step%97 != 0 {
			continue
		}
		want := make([]tree.ZoneKey, 0, len(present))
		for z := range present {
			want = append(want, z)
		}
		slices.SortFunc(want, spatialCompare)
		if got := spatialCount(root); got != len(want) {
			t.Fatalf("step %d: count %d, want %d", step, got, len(want))
		}
		for i, z := range want {
			if got, ok := spatialAt(root, i); !ok || got != z {
				t.Fatalf("step %d: rank %d is %v (%v), want %v", step, i, got, ok, z)
			}
		}
		if _, ok := spatialAt(root, len(want)); ok {
			t.Fatalf("step %d: a zone at rank %d of %d", step, len(want), len(want))
		}
	}
}

func sampleStore(t testing.TB, zones int) *Store {
	t.Helper()
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "sample.db"), "D", "test", 1<<28, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	holder, err := store.Endpoint("D")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	records := make([]Record, zones)
	for i := range records {
		records[i] = Record{Collection: fmt.Sprintf("collection-%05d", i%7), Location: fmt.Sprintf("z%05d", i), ID: "one", Payload: []byte(`{"v":1}`)}
	}
	if err := store.SeedLocalRecords(records); err != nil {
		t.Fatal(err)
	}
	return store
}

// PLAN-SAMPLE-1: a scale decision reads a bounded number of zones, spread
// over the address space, and learns how many zones the store holds. A store
// under the bound is read in full, exactly as PlanningStats reads it.
func TestPlanningSampleIsBoundedAndSpread(t *testing.T) {
	const zones, limit = 3000, 256
	store := sampleStore(t, zones)
	all, err := store.PlanningStats()
	if err != nil || len(all) != zones {
		t.Fatalf("%d zones, %v", len(all), err)
	}
	slices.SortFunc(all, func(a, b ZonePlanningStats) int { return spatialCompare(a.Zone, b.Zone) })
	rank := map[tree.ZoneKey]int{}
	for i, z := range all {
		rank[z.Zone] = i
	}
	sample, counted, err := store.PlanningSample(context.Background(), limit)
	if err != nil || counted != zones || len(sample) != limit {
		t.Fatalf("sample of %d, %d zones counted, %v; want %d and %d", len(sample), counted, err, limit, zones)
	}
	previous := -1
	for _, z := range sample {
		at, known := rank[z.Zone]
		if !known || z != all[at] {
			t.Fatalf("sampled %+v, the store holds %+v", z, all[at])
		}
		if gap := at - previous; gap < 1 || gap > zones/limit+1 {
			t.Fatalf("zone of rank %d follows rank %d: gap %d, want 1 to %d", at, previous, gap, zones/limit+1)
		}
		previous = at
	}
	whole, counted, err := store.PlanningSample(context.Background(), zones+10)
	if err != nil || counted != zones || !slices.Equal(whole, all) {
		t.Fatalf("a store under the bound: %d of %d zones, %v", len(whole), counted, err)
	}
	if _, _, err := store.PlanningSample(context.Background(), 0); err == nil {
		t.Fatal("a sample of no zone was accepted")
	}
	if !slices.IsSortedFunc(sample, func(a, b ZonePlanningStats) int { return bytes.Compare(a.Zone.ID[:], b.Zone.ID[:]) }) {
		t.Fatal("the sample is not in the order of zone ids")
	}
}

func BenchmarkPlanningInput(b *testing.B) {
	for _, zones := range []int{2000, 8000, 32000} {
		store := sampleStore(b, zones)
		b.Run(fmt.Sprintf("all/%d", zones), func(b *testing.B) {
			for b.Loop() {
				if _, err := store.PlanningStats(); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("sample/%d", zones), func(b *testing.B) {
			for b.Loop() {
				if _, _, err := store.PlanningSample(context.Background(), 512); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
