package indexus

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// TestBulkNodeCost ingests the batch directory $BULKBENCH_BATCH into one
// node and splits the time between the storage cut and the summary fold, and
// reports the heap the summaries keep. It does nothing without the variable.
func TestBulkNodeCost(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("BULKBENCH_BATCH")
	if dir == "" {
		t.Skip("BULKBENCH_BATCH names a staged batch")
	}
	descriptor := VNodeDescriptor{ID: ID{16}, Name: EncodeName(ID{16})}
	settings, err := NewSettings(descriptor.Name, 9000, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	n, err := NewNode(settings, testNewContact, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), descriptor.Name, "bench", 1<<36, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	holder, _ := store.Endpoint("http://127.0.0.1:9000")
	if err := store.InitCausal(descriptor, causal.Genesis{Cluster: "bench", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	net := &readPhaseTransport{nodes: map[string]*Node{holder.Host: n}}
	if err := n.EnableCausalCluster(&mesh.Service{Store: store, Self: holder, VNode: descriptor, Peers: net}); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stream := new(bytes.Buffer)
	if err := storage.PackBatch(stream, dir); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := n.mesh.service.Store.IngestBulkStream(context.Background(), stream, false)
	stream = nil
	if err != nil {
		t.Fatal(err)
	}
	ingest := time.Since(started)
	started = time.Now()
	if _, err := n.mesh.summary.Fold(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	fold := time.Since(started)
	runtime.GC()
	debug.FreeOSMemory()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	sizes := n.SummaryCacheSizes()
	fmt.Printf("{\"records\":%d,\"stream_and_ingest_s\":%.3f,\"fold_s\":%.3f,\"summary_zones\":%d,\"heap_live_before\":%d,\"heap_live_after\":%d,\"bytes_per_zone\":%d}\n",
		result.Records, ingest.Seconds(), fold.Seconds(), sizes.Zones, before.HeapAlloc, after.HeapAlloc, int64(after.HeapAlloc-before.HeapAlloc)/int64(max(sizes.Zones, 1)))
}
