package indexus

import (
	"context"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// TestConvergentReplicationNodeLifecycle verifies end-to-end convergent
// replication at the Node level: sync/ack replication, shadow isolation,
// degraded reads overlaying fallback on shadow, and automatic P2P repair via
// MaintainCausalCluster.
func TestConvergentReplicationNodeLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var descriptors []VNodeDescriptor
	for _, first := range []byte{255, 127, 63} {
		id := ID{first, 1}
		descriptors = append(descriptors, VNodeDescriptor{ID: id, Name: BASE64.Encode(id.Bytes())})
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	for i := 1; i < len(f.services); i++ {
		if err := f.services[i].Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range f.nodes {
		if err := n.EnableReplication(true, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range f.nodes {
		if err := n.meshMaintainContext(ctx); err != nil {
			t.Fatal(err)
		}
	}

	const col, loc = "Demo000000000000", "ab"
	r1 := storage.Record{Collection: col, Location: loc, ID: "k1", Metrics: []float64{10}}
	res, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "w-1", Record: &r1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Durability != mesh.DurabilityReplicated {
		t.Fatalf("Durability = %q, want %q", res.Durability, mesh.DurabilityReplicated)
	}

	z, err := tree.NewZoneKey(col, loc)
	if err != nil {
		t.Fatal(err)
	}
	routing, err := f.stores[0].CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	ownerHolder, err := routing.Route(z.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ownerNode, replicaNode *Node
	for i, svc := range f.services {
		if svc.Self == ownerHolder {
			ownerNode = f.nodes[i]
		} else if replicaNode == nil {
			replicaNode = f.nodes[i]
		}
	}
	if ownerNode == nil || replicaNode == nil {
		t.Fatalf("expected owner and replica nodes")
	}

	// Ensure shadow has caught up on replica and verify degraded read from shadow.
	if err := ownerNode.meshMaintainContext(ctx); err != nil {
		t.Fatal(err)
	}
	got, ok, err := replicaNode.DegradedRead(ctx, col, loc, "k1")
	if err != nil || !ok || got == nil || got.Metrics[0] != 10 {
		t.Fatalf("DegradedRead shadow k1 = (%+v, %v, %v), want 10", got, ok, err)
	}
	if replicaNode.durableStore().ShadowBytes() <= 0 {
		t.Fatalf("expected ShadowBytes > 0 on replica")
	}

	// Simulate owner silence > SuspectTimeout, accept fallback writes, verify degraded read.
	future := time.Now().Add(10 * time.Second)
	replicaNode.mesh.service.Now = func() time.Time { return future }
	r1Fb := storage.Record{Collection: col, Location: loc, ID: "k1", Metrics: []float64{77}}
	r2Fb := storage.Record{Collection: col, Location: loc, ID: "k2", Metrics: []float64{88}}
	if _, err := replicaNode.MeshData(ctx, mesh.Request{Kind: mesh.KindFallbackWrite, Owner: ownerHolder, RequestID: "fb-1", Record: &r1Fb}); err != nil {
		t.Fatal(err)
	}
	if _, err := replicaNode.MeshData(ctx, mesh.Request{Kind: mesh.KindFallbackWrite, Owner: ownerHolder, RequestID: "fb-2", Record: &r2Fb}); err != nil {
		t.Fatal(err)
	}
	gotFb, ok, err := replicaNode.DegradedRead(ctx, col, loc, "k1")
	if err != nil || !ok || gotFb == nil || gotFb.Metrics[0] != 77 {
		t.Fatalf("DegradedRead fallback k1 = (%+v, %v, %v), want 77", gotFb, ok, err)
	}

	// P2P maintenance tick automatically repairs fallback writes onto owner.
	if err := replicaNode.meshMaintainContext(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := ownerNode.durableStore().Snapshot(z)
	if err != nil || len(snap.Records) != 2 {
		t.Fatalf("owner snapshot after MaintainCausalCluster = %+v err=%v", snap, err)
	}
	vals := map[string]float64{}
	for _, r := range snap.Records {
		vals[r.ID] = r.Metrics[0]
	}
	if vals["k1"] != 77 || vals["k2"] != 88 {
		t.Fatalf("repaired values = %v, want k1=77 k2=88", vals)
	}
}
