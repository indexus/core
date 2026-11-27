package mockup

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// This drives the public Node and its durable causal runtime, including the
// per-zone messages used in production. Only delivery is simulated.
func TestCausalReadModesSurviveDroppedColdTransfersAndPartition(t *testing.T) {
	ResetNetwork()
	root := createConvergenceNode(t, indexus.EncodeName(indexus.ID{255}), 64, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const collection = "0000000000000001"
	for i := 0; i < 128; i++ {
		row := storage.Record{Collection: collection, Location: indexus.EncodeName(indexus.ID{byte(i * 2)}), ID: fmt.Sprint(i), Metrics: []float64{1}}
		if _, err := root.MeshData(ctx, mesh.Request{Kind: "write", RequestID: "seed:" + row.ID, Record: &row}); err != nil {
			t.Fatal(err)
		}
	}
	child := createConvergenceNode(t, indexus.EncodeName(indexus.ID{127}), 64, true, root)
	if err := root.Refresh(); err != nil {
		t.Fatal(err)
	}
	for _, node := range []*indexus.Node{root, child} {
		out, err := node.ReadContext(ctx, collection, "@", indexus.ReadOptions{Mode: indexus.ReadFresh})
		if err != nil || out.Set == nil || out.Set.Count() != 128 {
			t.Fatalf("warm: %+v %v", out, err)
		}
	}
	network.SetTransferDropRate(100)
	if err := child.CompactCausal(ctx, indexus.CausalCompactionConfig{}); err == nil {
		t.Fatal("cold transfer ignored injected drop")
	}
	value, _ := durableMeshes.Load(network)
	rpc := &causalRPC{network: network, runtime: value.(*durableNetwork), origin: root.Name()}
	for _, kind := range []string{"stage-zone", "install-zone"} {
		_, err := rpc.Exchange(ctx, fmt.Sprintf("http://%s:%d", child.Name(), child.Port()), mesh.Request{Kind: kind})
		if err == nil || !strings.Contains(err.Error(), "injected causal transfer drop") {
			t.Fatalf("%s bypassed simulation loss injection: %v", kind, err)
		}
	}
	_, dropped := network.TransferStats()
	if dropped == 0 {
		t.Fatal("production handoff path not fault-injected")
	}
	network.Partition([]*indexus.Node{root}, []*indexus.Node{child})
	for _, node := range []*indexus.Node{root, child} {
		for _, mode := range []indexus.ReadMode{indexus.ReadCached, indexus.ReadStaleIfError} {
			out, err := node.ReadContext(ctx, collection, "@", indexus.ReadOptions{Mode: mode, MaxAge: time.Minute})
			if err != nil || out.Set == nil || out.Set.Count() != 128 || out.Source != "cache" {
				t.Fatalf("partition %s: %+v %v", mode, out, err)
			}
		}
	}
	network.Heal()
	network.SetTransferDropRate(0)
	if err := child.CompactCausal(ctx, indexus.CausalCompactionConfig{}); err != nil {
		t.Fatal(err)
	}
	out, err := child.ReadContext(ctx, collection, "@", indexus.ReadOptions{Mode: indexus.ReadFresh})
	if err != nil || out.Set == nil || out.Set.Count() != 128 || out.Source != "inventory" {
		t.Fatalf("recovered fresh: %+v %v", out, err)
	}
	ok, dropped := network.TransferStats()
	if ok == 0 || dropped == 0 {
		t.Fatal("transfer counters did not exercise both paths")
	}
}
