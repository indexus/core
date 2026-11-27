package indexus

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// One record per zone, like the loaded DVF collection. Payloads deliberately
// dominate the fixture: derived sums must not retain or copy their contents.
func BenchmarkSummaryInventoryPayload(b *testing.B) {
	n := newMemoryBenchNode(b)
	payload := json.RawMessage(`{"detail":"` + strings.Repeat("x", 16<<10) + `"}`)
	for i := 0; i < 1024; i++ {
		r := storage.Record{Collection: "MemoryBench00001", Location: fmt.Sprintf("a%06d", i), ID: fmt.Sprint(i), Metrics: []float64{1, 2, 3, 4, 5}, Payload: payload}
		if _, _, _, err := n.durableStore().ApplyRecord(fmt.Sprint(i), r); err != nil {
			b.Fatal(err)
		}
	}
	view := n.mesh.summary.View()
	if _, err := n.mesh.summary.FoldLocal(context.Background(), view); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := n.mesh.summary.FoldLocal(context.Background(), view); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkMonitoringZoneCount(b *testing.B) {
	n := newMemoryBenchNode(b)
	for i := 0; i < 4096; i++ {
		r := storage.Record{Collection: "MemoryBench00001", Location: fmt.Sprintf("a%06d", i), ID: fmt.Sprint(i), Metrics: []float64{1}}
		if _, _, _, err := n.durableStore().ApplyRecord(fmt.Sprint(i), r); err != nil {
			b.Fatal(err)
		}
	}
	// Warm the exact committed version before measuring idle maintenance.
	if _, err := n.Count(); err != nil {
		b.Fatal(err)
	}
	b.Run("materialized", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			inventory, err := n.durableStore().PlanningStats()
			if err != nil {
				b.Fatal(err)
			}
			total := 0
			collections := map[string]struct{}{}
			for _, row := range inventory {
				total += int(row.Items)
				collections[row.Zone.Collection] = struct{}{}
			}
			n.measuredOwnership.Store(&measuredOwnership{collections: len(collections), zones: len(inventory)})
			n.items.Store(int64(total))
		}
	})
	b.Run("versioned", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := n.Count(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// Fsync is excluded only from the benchmark fixture setup, never production.
func newMemoryBenchNode(b testing.TB) *Node {
	b.Helper()
	settings, err := NewSettings("AAAAAAAAAAAAAADB", 21000, time.Second)
	if err != nil {
		b.Fatal(err)
	}
	settings.SetAdvertise("127.0.0.1")
	n, err := NewNode(settings, testNewContact, nil)
	if err != nil {
		b.Fatal(err)
	}
	s, err := storage.OpenWithOptions(filepath.Join(b.TempDir(), "memory.db"), n.Name(), "memory-audit", 1<<28, storage.Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	holder, err := s.Endpoint("http://127.0.0.1:21000")
	if err != nil {
		b.Fatal(err)
	}
	descriptor := VNodeDescriptor{Name: n.Name(), ID: FromSlice(n.ID())}
	if err = s.InitCausal(descriptor, causal.Genesis{Cluster: "memory-audit", Holder: holder}); err != nil {
		b.Fatal(err)
	}
	transport := &readPhaseTransport{nodes: map[string]*Node{holder.Host: n}}
	if err = n.EnableCausalCluster(&mesh.Service{Store: s, Self: holder, VNode: descriptor, Peers: transport}); err != nil {
		b.Fatal(err)
	}
	return n
}

func TestInventoryBatchFoldRetainsDirectRowsAndSharedAncestors(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 5000)
	rows := []storage.Record{
		{Collection: "Demo000000000000", Location: "a", ID: "parent", Metrics: []float64{5, 2}},
		{Collection: "Demo000000000000", Location: "ab", ID: "child", Metrics: []float64{7, 3}},
		{Collection: "Demo000000000000", Location: "ac", ID: "sibling", Metrics: []float64{11, 4}},
	}
	for i, r := range rows {
		if _, _, _, err := n.durableStore().ApplyRecord(fmt.Sprintf("batch-%d", i), r); err != nil {
			t.Fatal(err)
		}
	}
	for pass := 0; pass < 2; pass++ {
		if complete, err := n.mesh.summary.FoldLocal(context.Background(), n.mesh.summary.View()); err != nil || !complete {
			t.Fatalf("fold: %v %v", complete, err)
		}
		for _, location := range []string{"a", BASE64.Root()} {
			got := n.mesh.summary.Owned("Demo000000000000", location)
			if !got.IsEqual(wire.NewAbelian(3, []float64{23, 9})) {
				t.Fatalf("pass %d location %s: %#v", pass, location, got)
			}
		}
	}
}

// Owned sums read from zonelog keep the compensation of the store (Neumaier
// pairs) and ignore payloads and tombstones.
func TestSummaryMetricsFoldPreservesCompensationAndIgnoresPayload(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 5000)
	values := []float64{1e16, 1, -1e16}
	// Deliberately write IDs backwards; folding must still use canonical ID order.
	for i := len(values) - 1; i >= 0; i-- {
		r := storage.Record{Collection: "Demo000000000000", Location: "ab", ID: fmt.Sprint(i), Metrics: []float64{values[i]}, Payload: json.RawMessage(`{"irrelevant":[1,2,3]}`)}
		if _, _, _, err := n.durableStore().ApplyRecord(fmt.Sprint(i), r); err != nil {
			t.Fatal(err)
		}
	}
	deleted := storage.Record{Collection: "Demo000000000000", Location: "ab", ID: "gone", Deleted: true, Metrics: []float64{123}}
	if _, _, _, err := n.durableStore().ApplyRecord("deleted", deleted); err != nil {
		t.Fatal(err)
	}
	if complete, err := n.mesh.summary.FoldLocal(context.Background(), n.mesh.summary.View()); err != nil || !complete {
		t.Fatalf("fold failed %v %v", complete, err)
	}
	got := n.mesh.summary.Owned("Demo000000000000", "@")
	if got.Count() != 3 || got.Metric(0) != 1 {
		t.Fatalf("compensation or tombstone changed: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := n.mesh.summary.FoldLocal(ctx, n.mesh.summary.View()); err == nil {
		t.Fatal("cancelled fold succeeded")
	}
	if after := n.mesh.summary.Owned("Demo000000000000", "@"); !got.IsEqual(after) {
		t.Fatal("failed fold replaced existing totals")
	}
}
