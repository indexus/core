package indexus

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// Once a zone has flipped to the receiver of a transfer, its writes land
// there while the source still serves its own copy until the region
// installs. A read through the source must also ask the receiver, or it
// loses a write acknowledged before it started (READ-1; 2026-11-20: 8 of 57
// runs of the continuous-reads test under load).
func TestReadDuringTransferSeesWritesOfFlippedZones(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	collection := "0000000000000001"
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = BASE64.Encode(root.ID.Bytes())
	zone, _ := NewZoneKey(collection, "0")
	target := VNodeDescriptor{ID: zone.ID}
	target.Name = BASE64.Encode(target.ID.Bytes())
	fixture := newReadPhaseFixture(t, []VNodeDescriptor{root, target}, false, false)
	nodes, services := fixture.nodes, fixture.services
	seed := storage.Record{Collection: collection, Location: "0", ID: "before", Metrics: []float64{1}}
	if _, err := nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "before", Record: &seed}); err != nil {
		t.Fatal(err)
	}
	var wrote, probed atomic.Bool
	var failure atomic.Value
	// The zone has just flipped to the receiver: it takes the next write.
	fixture.net.after = func(r mesh.Request) error {
		if r.Kind != "install-zone" || !wrote.CompareAndSwap(false, true) {
			return nil
		}
		late := storage.Record{Collection: collection, Location: "0", ID: "after-flip", Metrics: []float64{2}}
		response, err := nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "after-flip", Record: &late})
		if err != nil {
			failure.Store("write after the flip: " + err.Error())
		} else if response.AppliedBy != services[1].Self {
			failure.Store("the write after the flip was not applied by the receiver")
		}
		return nil
	}
	// The source emitted the region; the receiver has not installed it, and
	// passes reads on to the source, whose copy predates that write.
	fixture.net.before = func(r mesh.Request) error {
		if r.Kind != "install" || !wrote.Load() || !probed.CompareAndSwap(false, true) {
			return nil
		}
		for i, service := range services {
			inventory, err := service.Query(ctx)
			if err != nil {
				failure.Store(fmt.Sprintf("read through node %d: %v", i, err))
				return nil
			}
			found := 0
			for _, snapshot := range inventory {
				for _, row := range snapshot.Records {
					if row.ID == "after-flip" || row.ID == "before" {
						found++
					}
				}
			}
			if found != 2 {
				failure.Store(fmt.Sprintf("read through node %d during the transfer: %d of the 2 acknowledged rows", i, found))
			}
		}
		return nil
	}
	if err := services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if !probed.Load() {
		t.Fatal("no zone flipped before the region: the case was not exercised")
	}
	if msg := failure.Load(); msg != nil {
		t.Fatal(msg)
	}
}
