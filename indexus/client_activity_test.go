package indexus

import (
	"context"
	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoteClientReadAndMaintenanceShareInventoryWithoutSharingDemand(t *testing.T) {
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, true)
	ctx := context.Background()
	var marked, unmarked atomic.Int64
	f.net.before = func(r mesh.Request) error {
		if r.Kind == mesh.KindInventoryPrefix {
			if r.ClientWork {
				marked.Add(1)
			} else {
				unmarked.Add(1)
			}
		}
		return nil
	}
	if _, err := f.nodes[1].GetRecordsContext(ctx, "Collection000000", "aa", 100); err != nil {
		t.Fatal(err)
	}
	for _, node := range f.nodes {
		if arrivals, active := node.mesh.clientActivity.Sample(); arrivals < 1 || active != 0 {
			t.Fatalf("node %s client reads=%d active=%d", node.Name(), arrivals, active)
		}
	}
	if marked.Load() == 0 || unmarked.Load() != 0 {
		t.Fatalf("client inventory: marked=%d unmarked=%d", marked.Load(), unmarked.Load())
	}
	if _, err := f.services[1].QueryPrefix(ctx, "Collection000000", "aa"); err != nil {
		t.Fatal(err)
	}
	if unmarked.Load() == 0 {
		t.Fatal("maintenance did not use inventory")
	}
	for _, node := range f.nodes {
		if arrivals, active := node.mesh.clientActivity.Sample(); arrivals != 0 || active != 0 {
			t.Fatalf("maintenance became foreground work: %d %d", arrivals, active)
		}
	}
}

func TestClientDemandTracksQuietLongReadsAndConcurrentTicks(t *testing.T) {
	descriptor := VNodeDescriptor{ID: ID{16}}
	descriptor.Name = EncodeName(descriptor.ID)
	f := newReadPhaseFixture(t, []VNodeDescriptor{descriptor}, false, true)
	n := f.nodes[0]
	n.EnableAutoscale(autoscale.Config{Enabled: true, ItemsOnly: true, Sample: func(string) autoscale.ResourceSample { return autoscale.ResourceSample{} }})
	if in := n.sampleAutoscaleDemand(); in.ClientDemand != autoscale.ClientQuiet {
		t.Fatal(in)
	}
	_, finish := n.mesh.clientActivity.Begin(context.Background())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); n.AutoscaleTick() }()
	}
	wg.Wait()
	if in := n.sampleAutoscaleDemand(); in.ClientDemand != autoscale.ClientActive || in.ClientInFlight != 1 {
		t.Fatal(in)
	}
	finish()
	if in := n.sampleAutoscaleDemand(); in.ClientDemand != autoscale.ClientActive || in.ClientInFlight != 0 {
		t.Fatal(in)
	}
	n.mesh.sampleMu.Lock()
	n.mesh.lastClientSample = time.Now().Add(-time.Hour)
	n.mesh.sampleMu.Unlock()
	if in := n.sampleAutoscaleDemand(); in.ClientDemand != autoscale.ClientQuiet {
		t.Fatal(in)
	}
}
