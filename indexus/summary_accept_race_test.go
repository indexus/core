package indexus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// A contribution of the old holder that passed the unlocked holder check,
// then waited for Tree.Mu while the child MOVEd and the new holder's
// contribution was accepted, must not overwrite it (8e7b43b, 742c1db).
func TestSummaryOldContributionPausedBeforeLockCannotOverwriteNewHolder(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
	defer func() {
		for _, n := range f.nodes {
			n.mesh.summary.Mu.Lock()
			n.mesh.summary.StopFlushLocked()
			n.mesh.summary.Mu.Unlock()
		}
	}()
	if err := f.nodes[1].JoinCausalCluster(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	for _, n := range f.nodes {
		if err := n.meshRefreshView(); err != nil {
			t.Fatal(err)
		}
	}
	parent, oldChild, newChild := f.nodes[0], f.nodes[1], f.nodes[2]
	const collection = "0000000000000001"
	location := ""
	for _, candidate := range ChildCandidates(BASE64, "@") {
		if oldChild.mesh.summary.OwnsHere(collection, candidate) {
			location = candidate
			break
		}
	}
	if location == "" {
		t.Fatal("no child location")
	}
	record := storage.Record{Collection: collection, Location: location, ID: "before", Metrics: []float64{10}}
	if _, err := oldChild.MeshData(ctx, mesh.Request{Kind: "write", RequestID: "before", Record: &record}); err != nil {
		t.Fatal(err)
	}
	oldWire := oldChild.mesh.summary.Export(collection, []string{location}, "")[0]

	// The old contribution passes the unlocked check, then stops before Tree.Mu.
	paused, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	parent.mesh.summary.SetBeforeAcceptLockForTest(func(in mesh.Contribution) {
		// Only the old holder's contribution waits; every other one goes on.
		if in.Holder == oldWire.Holder && in.Location == location && first.CompareAndSwap(false, true) {
			close(paused)
			<-release
		}
	})
	accepted := make(chan error, 1)
	go func() { accepted <- parent.mesh.summary.AcceptWire(oldWire) }()
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("old contribution never reached the lock")
	}

	// Meanwhile the child MOVEs and the new holder's contribution lands.
	if err := f.services[2].ReceiveMove(ctx, f.services[1].Self.Host); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[2].Self.Host}); err != nil {
		t.Fatal(err)
	}
	for _, n := range f.nodes {
		if err := n.meshRefreshView(); err != nil {
			t.Fatal(err)
		}
	}
	if !newChild.mesh.summary.Ensure(context.Background()) {
		t.Fatal("new placement fold failed")
	}
	record.ID, record.Metrics = "after", []float64{1}
	if _, err := newChild.MeshData(ctx, mesh.Request{Kind: "write", RequestID: "after", Record: &record}); err != nil {
		t.Fatal(err)
	}
	if _, err := newChild.mesh.summary.Fold(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	newWire := newChild.mesh.summary.Export(collection, []string{location}, "")[0]
	if oldWire.Holder.Storage == newWire.Holder.Storage {
		t.Fatal("test requires a MOVE replacing storage")
	}
	if err := parent.mesh.summary.AcceptWire(newWire); err != nil {
		t.Fatal(err)
	}
	if got := parent.mesh.summary.Owned(collection, "@").Count(); got != 2 {
		t.Fatalf("new placement count=%d, want 2", got)
	}

	close(release)
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	if got := parent.mesh.summary.Owned(collection, "@").Count(); got != 2 {
		t.Fatalf("paused old contribution overwrote the new holder: count=%d, want 2", got)
	}
}
