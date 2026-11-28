package indexus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// AutoscaleTick evaluates the machine, not only this process, and the scale
// request it posts says so. The sampler is injected: nothing reads /proc.
func TestAutoscaleTickPostsHostPressureForAHotHost(t *testing.T) {
	d := VNodeDescriptor{ID: ID{16}}
	d.Name = EncodeName(d.ID)
	n := newReadPhaseFixture(t, []VNodeDescriptor{d}, false, false).nodes[0]
	for _, loc := range []string{"0", "y"} {
		for i := 0; i < 8; i++ {
			id := fmt.Sprintf("%s-%d", loc, i)
			if err := n.New(&Item{Collection: "0000000000000001", Location: loc, Id: id, RequestID: id}, "@", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	demand := n.WriteDemand()
	if demand["arrived"] != int64(16) || demand["acknowledged"] != int64(16) || demand["in_flight"] != int64(0) {
		t.Fatalf("demand after 16 served writes: %v", demand)
	}

	posted := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		posted <- body
	}))
	defer server.Close()
	// This vnode uses 45 % of a machine that is at 90 %: under its own 60 %.
	sample := autoscale.ResourceSample{CPUPct: 90, CPUOK: true, ProcessCPUPct: 45, HostCPUPct: 90, HostCPUOK: true, DiskFreePct: 80, DiskOK: true}
	n.EnableAutoscale(autoscale.Config{Enabled: true, ProvisionerURL: server.URL, DisableDiskSignal: true, Cooldown: time.Hour,
		PressureHold: time.Millisecond, Sample: func(string) autoscale.ResourceSample { return sample }})
	n.MeasureItems()
	n.AutoscaleTick()
	time.Sleep(5 * time.Millisecond)
	n.AutoscaleTick()
	select {
	case body := <-posted:
		if body["reason"] != autoscale.ReasonHostPressure || body["signal"] != "cpu" || body["transfers_in_flight"] != 0.0 || body["cpu_process_pct"] != 45.0 || body["cpu_host_pct"] != 90.0 {
			t.Fatalf("scale request: %v", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no scale request for a host at 90%%: %v", n.AutoscaleSnapshot())
	}
}

func TestAutoscaleSamplesInboundMountsOnce(t *testing.T) {
	var descriptors []VNodeDescriptor
	for _, id := range []ID{{16}, {224}} {
		descriptors = append(descriptors, VNodeDescriptor{ID: id, Name: BASE64.Encode(id.Bytes())})
	}
	f := newReadPhaseFixture(t, descriptors, false, true)
	ctx := context.Background()
	row := storage.Record{Collection: "0000000000000001", Location: tree.EncodeName(tree.ID{224}), ID: "join-row"}
	if _, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "seed", Record: &row}); err != nil {
		t.Fatal(err)
	}
	if err := f.services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	receiver := f.nodes[1]
	receiver.EnableAutoscale(autoscale.Config{Enabled: true, ItemsOnly: true, Sample: func(string) autoscale.ResourceSample { return autoscale.ResourceSample{} }})
	receiver.AutoscaleTick()
	if got := receiver.AutoscaleSnapshot()["pressure"].(map[string]any)["inbound_lots"].(int64); got < 1 {
		t.Fatalf("no inbound lot on receiver, got %d", got)
	}
	if _, err := receiver.MeshData(ctx, mesh.Request{Kind: "install-zone"}); err == nil {
		t.Fatal("invalid lot acknowledged")
	}
	receiver.AutoscaleTick()
	if got := receiver.AutoscaleSnapshot()["pressure"].(map[string]any)["inbound_lots"].(int64); got != 0 {
		t.Fatalf("failed or old lot counted, got %d", got)
	}
}
