package indexus

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
)

// REM-112: a write that lands in a boundary while that boundary's value is
// being pushed to the parent's owner marks the edge stale. The push then
// succeeds with the older value; the edge must stay queued so the next flush
// sends the newer one, instead of waiting for the next write or poll.
func TestWriteDuringEdgePushIsSentNext(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
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
	t.Cleanup(func() {
		f.net.before = nil
		for _, n := range f.nodes {
			n.mesh.summary.Mu.Lock()
			n.mesh.summary.StopFlushLocked()
			n.mesh.summary.Mu.Unlock()
		}
	})
	const collection = "0000000000000001"
	view := f.nodes[0].mesh.summary.View()
	root, _ := NewZoneKey(collection, "@")
	rootOwner, _ := view.OwnerName(root.ID)
	parent, child := f.nodes[0], f.nodes[1]
	if parent.Name() != rootOwner {
		parent, child = child, parent
	}
	// Two records below one depth-1 zone the child holds: one boundary.
	var first, second string
	for _, a := range ChildCandidates(BASE64, "@") {
		candidates := ChildCandidates(BASE64, a)
		x, y := candidates[0]+"0", candidates[1]+"0"
		owners := map[string]bool{}
		for _, location := range []string{a, x, y} {
			zone, _ := NewZoneKey(collection, location)
			owner, _ := view.OwnerName(zone.ID)
			owners[owner] = true
		}
		if len(owners) == 1 && owners[child.Name()] {
			first, second = x, y
			break
		}
	}
	if first == "" {
		t.Fatal("no depth-1 zone held by the child with its descendants")
	}

	var once sync.Once
	injected := make(chan error, 1)
	f.net.before = func(r mesh.Request) error {
		if r.Kind == mesh.KindSummaryContribute {
			once.Do(func() {
				injected <- child.New(&Item{Collection: collection, Location: second, Id: "second", Metrics: []float64{1}}, "@", nil)
			})
		}
		return nil
	}
	if err := child.New(&Item{Collection: collection, Location: first, Id: "first", Metrics: []float64{1}}, "@", nil); err != nil {
		t.Fatal(err)
	}
	child.mesh.summary.FlushPendingEdges()
	select {
	case err := <-injected:
		if err != nil {
			t.Fatalf("write during the push: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no summary push reached the parent")
	}
	// The poll that would repair this runs every 5 s or more and is not
	// started here: only the push path can deliver the second write.
	deadline := time.Now().Add(3 * time.Second)
	for {
		child.mesh.summary.FlushPendingEdges()
		got := parent.mesh.summary.Owned(collection, "@")
		if got != nil && got.Count() == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent root after a write during the push: %v, want 2 records", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An edge whose value did not change is not sent again to the holder that
// already acknowledged it (Edge.sent): after a split every boundary is
// queued again, and most of them carry the value their parent already has.
func TestEdgeWithTheValueItsParentHoldsIsNotPushedAgain(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
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
	var pushes atomic.Int64
	f.net.before = func(r mesh.Request) error {
		if r.Kind == mesh.KindSummaryContribute {
			pushes.Add(1)
		}
		return nil
	}
	t.Cleanup(func() {
		f.net.before = nil
		for _, n := range f.nodes {
			n.mesh.summary.Mu.Lock()
			n.mesh.summary.StopFlushLocked()
			n.mesh.summary.Mu.Unlock()
		}
	})
	const collection = "0000000000000001"
	view := f.nodes[0].mesh.summary.View()
	root, _ := NewZoneKey(collection, "@")
	rootOwner, _ := view.OwnerName(root.ID)
	parent, child := f.nodes[0], f.nodes[1]
	if parent.Name() != rootOwner {
		parent, child = child, parent
	}
	var location string
	for _, a := range ChildCandidates(BASE64, "@") {
		below := ChildCandidates(BASE64, a)[0] + "0"
		held := true
		for _, l := range []string{a, below} {
			zone, _ := NewZoneKey(collection, l)
			owner, _ := view.OwnerName(zone.ID)
			held = held && owner == child.Name()
		}
		if held {
			location = below
			break
		}
	}
	if location == "" {
		t.Fatal("no depth-1 zone held by the child with its descendants")
	}
	item := &Item{Collection: collection, Location: location, Id: "same", Metrics: []float64{1}}
	if err := child.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		child.mesh.summary.FlushPendingEdges()
		if got := parent.mesh.summary.Owned(collection, "@"); got != nil && got.Count() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first value never reached the parent")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Drain what the first delivery left queued, then write the same record
	// again: the edge is stale, its value is the one the parent holds.
	time.Sleep(100 * time.Millisecond)
	child.mesh.summary.FlushPendingEdges()
	sent, unchanged := pushes.Load(), child.mesh.summary.Stats().Unchanged
	if err := child.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the explicit flush below processes the dirty mark
	child.mesh.summary.FlushPendingEdges()
	if got := pushes.Load(); got != sent {
		t.Fatalf("an unchanged value was pushed again: %d pushes, want %d", got, sent)
	}
	if got := child.mesh.summary.Stats().Unchanged; got <= unchanged {
		t.Fatalf("no edge recognised as unchanged (%d)", got)
	}
	if got := parent.mesh.summary.Owned(collection, "@"); got == nil || got.Count() != 1 {
		t.Fatalf("parent root: %v, want 1 record", got)
	}

	// Lose only the parent's volatile runtime, after its ACK: the child still
	// remembers that this host received the value. This is not a disk rollback.
	parent.mesh.summary.Mu.Lock()
	parent.mesh.summary.StopFlushLocked()
	parent.mesh.summary.Mu.Unlock()
	fresh, err := NewNode(parent.settings, testNewContact, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := parent.mesh.service
	service := &mesh.Service{Store: old.Store, Self: old.Self, VNode: old.VNode, Peers: f.net}
	if err := fresh.EnableCausalCluster(service); err != nil {
		t.Fatal(err)
	}
	f.net.nodes[service.Self.Host] = fresh
	t.Cleanup(func() {
		fresh.mesh.summary.Mu.Lock()
		fresh.mesh.summary.StopFlushLocked()
		fresh.mesh.summary.Mu.Unlock()
	})
	if got := fresh.mesh.summary.Owned(collection, "@").Count(); got != 0 {
		t.Fatalf("fault did not lose the volatile contribution: got %d", got)
	}
	sent, unchanged = pushes.Load(), child.mesh.summary.Stats().Unchanged
	if err := child.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	child.mesh.summary.FlushPendingEdges()
	if pushes.Load() != sent || child.mesh.summary.Stats().Unchanged <= unchanged {
		t.Fatal("fixture must exercise unchanged suppression after the parent lost its cache")
	}
	if got := fresh.mesh.summary.Owned(collection, "@").Count(); got != 0 {
		t.Fatalf("a push repaired the cache before the poll: %d", got)
	}
	for range 2 {
		if err := fresh.mesh.summary.PollRemoteHosts(ctx, true); err != nil {
			t.Fatal(err)
		}
		if got := fresh.mesh.summary.Owned(collection, "@"); got.Count() != 1 || got.Metric(0) != 1 {
			t.Fatalf("poll did not repair the lost contribution exactly once: %v", got)
		}
	}
}
