package indexus

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestAutomaticDurableCompactionAfterInsertions(t *testing.T) {
	t.Parallel()
	runAutomaticCompaction(t, false)
}

// COLD-2: the same 1 -> 4 -> 1 cycle while writes keep trickling in, spread
// over every endpoint, under the cool rate. Requiring zero writes instead
// (CoolWriteRate near 0) leaves the cluster at four vnodes.
func TestCOLD2_TepidClusterStillCompacts(t *testing.T) {
	t.Parallel()
	runAutomaticCompaction(t, true)
}

func runAutomaticCompaction(t *testing.T, trickle bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var descriptors []VNodeDescriptor
	for _, first := range []byte{255, 127, 63, 31} {
		id := ID{first, 1}
		descriptors = append(descriptors, VNodeDescriptor{ID: id, Name: BASE64.Encode(id.Bytes())})
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	const count = 1024
	for i := 0; i < count; i++ {
		row := storage.Record{Collection: "0000000000000001", Location: tree.EncodeName(tree.ID{byte(i % 256)}), ID: fmt.Sprintf("row-%d", i), Metrics: []float64{1}}
		if _, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "seed:" + row.ID, Record: &row}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < len(f.services); i++ {
		if err := f.services[i].Join(ctx); err != nil {
			t.Fatal(err)
		}
		for _, s := range f.services[:i+1] {
			if err := s.Sync(ctx, []string{f.services[i].Self.Host, f.services[0].Self.Host}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, s := range f.services {
		if err := s.Sync(ctx, []string{f.services[0].Self.Host}); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := f.stores[0].CausalRouting()
	if before.Current().Len() != 4 {
		t.Fatalf("initial scale=%d", before.Current().Len())
	}
	for _, node := range f.nodes {
		config := CausalCompactionConfig{CoolFor: 10 * time.Millisecond, Interval: 10 * time.Millisecond, Timeout: 15 * time.Second}
		if trickle {
			// Cool under 200 writes/s for at least one measured second.
			config = CausalCompactionConfig{CoolFor: 300 * time.Millisecond, Interval: 10 * time.Millisecond, Timeout: 15 * time.Second, CoolWriteRate: 200}
		}
		if err := node.EnableCausalCompaction(config); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, node := range f.nodes {
			node.DisableCausalCompaction()
		}
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			busy := false
			for _, node := range f.nodes {
				busy = busy || node.mesh.compactionRunning.Load()
			}
			if !busy {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Error("compaction worker did not stop")
	}()
	extra := map[string]bool{}
	tick := 0
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		for _, node := range f.nodes {
			_ = node.Refresh()
			_ = node.Update()
		}
		if trickle {
			// One write per endpoint per pass: tepid, not silent.
			for _, node := range f.nodes {
				tick++
				row := storage.Record{Collection: "0000000000000001", Location: tree.EncodeName(tree.ID{byte(tick % 256)}), ID: fmt.Sprintf("tepid-%d", tick), Metrics: []float64{1}}
				if _, err := node.MeshData(ctx, mesh.Request{Kind: "write", RequestID: "tepid:" + row.ID, Record: &row}); err == nil {
					extra[row.ID] = true
				}
			}
		}
		snapshot, _ := f.stores[0].CausalRouting()
		if snapshot.Current().Len() == 1 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	for i, node := range f.nodes {
		// Stop initiating new work before final convergence checks.
		node.DisableCausalCompaction()
		if err := node.Refresh(); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
		if err := node.Update(); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := f.stores[i].CausalRouting()
		if snapshot.Current().Len() != 1 {
			r := node.mesh.lastColdResult.Load()
			t.Fatalf("scale stayed %d at %d, last=%+v", snapshot.Current().Len(), i, r)
		}
		inventory, err := f.services[i].Query(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, snap := range inventory {
			for _, row := range snap.Records {
				if row.Metrics[0] != 1 || got[row.ID] {
					t.Fatal("changed or duplicate payload")
				}
				got[row.ID] = true
			}
		}
		for id := range extra {
			if !got[id] {
				t.Fatalf("acknowledged tepid write %s lost", id)
			}
		}
		if len(got) < count+len(extra) {
			t.Fatalf("rows=%d want %d", len(got), count+len(extra))
		}
		_, set, err := node.Get("0000000000000001", "@", true, nil, true)
		if err != nil || set == nil || set.Count() != len(got) || set.Abelian().Metric(0) != float64(len(got)) {
			t.Fatalf("aggregate %d: %v %v", i, set, err)
		}
	}
	t.Logf("durable scale 1 -> 4 -> 1, %d rows (+%d tepid writes acknowledged during folds), exact reads and aggregates through all four endpoints", count, len(extra))
}
