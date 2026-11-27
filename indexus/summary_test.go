package indexus

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type refusingTransport struct{ calls int }

func (r *refusingTransport) Exchange(context.Context, string, mesh.Request) (mesh.Response, error) {
	r.calls++
	return mesh.Response{}, errors.New("no network expected")
}

// A contribution whose parent resolves to this very vnode (the parent moved
// here between scheduling and send) is folded locally. The service's Handle
// ignores summary kinds, so routing it through Exchange would drop it while
// reporting success, and an empty parent zone would never be created.
func TestPushContributionToSelfIsAcceptedLocally(t *testing.T) {
	t.Parallel()
	settings, err := NewSettings("AAAAAAAAAAAAAAAB", 21000, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	node, err := NewNode(settings, testNewContact, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), node.Name(), "test", 1<<26, testStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	holder, err := store.Endpoint("http://self:1")
	if err != nil {
		t.Fatal(err)
	}
	vnode := VNodeDescriptor{Name: node.Name(), ID: FromSlice(node.ID())}
	if err := store.InitCausal(vnode, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	peers := &refusingTransport{}
	if err := node.EnableCausalCluster(&mesh.Service{Store: store, Self: holder, VNode: vnode, Peers: peers}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three"} {
		if _, err := store.Write(id, storage.Record{Collection: "Demo000000000000", Location: "aa", ID: id, Metrics: []float64{1}}); err != nil {
			t.Fatal(err)
		}
	}
	in := mesh.Contribution{Collection: "Demo000000000000", Location: "aa", Revision: 1, Holder: holder, Count: 3, Metrics: []float64{3}}
	// What FlushPendingEdges does for an edge whose parent resolves here.
	replica, err := node.mesh.service.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	parent, err := NewZoneKey(in.Collection, BASE64.Parent(in.Location))
	if err != nil {
		t.Fatal(err)
	}
	if target, err := replica.Route(parent.ID); err != nil || target != node.mesh.service.Self {
		t.Fatalf("parent holder %v, %v: want self", target, err)
	}
	if err := node.mesh.summary.AcceptWire(in); err != nil {
		t.Fatal(err)
	}
	if got := node.mesh.summary.Owned("Demo000000000000", "a"); got.Count() != 3 || got.Metric(0) != 3 {
		t.Fatalf("parent after self push: %v", got)
	}
	if got := node.mesh.summary.Owned("Demo000000000000", "@"); got.Count() != 3 {
		t.Fatalf("root after self push: %v", got)
	}
	if peers.calls != 0 {
		t.Fatalf("self push used the network %d times", peers.calls)
	}

	// A drill must read that summary. The compact cache projects the skipped
	// unary prefix; every record sits at aa, so the root names it aa.
	_, set, err := node.meshGet(context.Background(), "Demo000000000000", "@", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if set == nil || set.Count() != 3 {
		t.Fatalf("root set count=%v", set)
	}
	child, ok := set.Get("aa")
	if !ok || child.Count() != 3 || child.Metric(0) != 3 || len(set.List()) != 1 {
		t.Fatalf("root child aa: ok=%v value=%v", ok, child)
	}
	_, nested, err := node.meshGet(context.Background(), "Demo000000000000", "a", true, false)
	if err != nil {
		t.Fatal(err)
	}
	leaf, ok := nested.Get("aa")
	if !ok || leaf.Count() != 3 {
		t.Fatalf("child aa: ok=%v value=%v", ok, leaf)
	}
}

// After a topology change the children of a moved parent did not change, so
// only a re-push driven by the view change can reach the new parent owner
// before the slow poll does. Zones whose parent owner did not move stay quiet.
func TestRepushBoundariesMarksOnlyMovedParentEdges(t *testing.T) {
	t.Parallel()
	// This vnode's ID is exactly the zone id of leaf "aa", so it keeps that
	// leaf whatever else joins; J's ID is exactly the zone id of parent "a".
	leafA, err := NewZoneKey("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := NewSettings(EncodeName(leafA.ID), 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	node, err := NewNode(settings, testNewContact, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), node.Name(), "test", 1<<26, testStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	holder, err := store.Endpoint("http://self:1")
	if err != nil {
		t.Fatal(err)
	}
	self := VNodeDescriptor{Name: node.Name(), ID: FromSlice(node.ID())}
	if err := store.InitCausal(self, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	if err := node.EnableCausalCluster(&mesh.Service{Store: store, Self: holder, VNode: self, Peers: &refusingTransport{}}); err != nil {
		t.Fatal(err)
	}
	// Leaf "aa" (parent "a") and leaf "aab" (parent "aa"). Before: everything
	// is owned here. After: a vnode J whose ID is exactly the zone id of
	// parent "a" appears, so "a" moves to J; parent "aa" is this vnode's own
	// ID and stays here.
	for _, location := range []string{"aa", "aab"} {
		if err := node.New(&Item{Collection: "Demo000000000000", Location: location, Id: location, Metrics: []float64{1}}, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	previous := node.mesh.summary.View()
	current := tree.NewView(0)
	if err := current.Add(self.Name, self.ID); err != nil {
		t.Fatal(err)
	}
	parentA, err := NewZoneKey("Demo000000000000", "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Add("J", parentA.ID); err != nil {
		t.Fatal(err)
	}
	if owner, _ := current.OwnerName(parentA.ID); owner != "J" {
		t.Fatalf("fixture: parent a owned by %s", owner)
	}
	if owner, _ := current.OwnerName(leafA.ID); owner != self.Name {
		t.Fatalf("fixture: leaf aa owned by %s", owner)
	}
	node.mesh.summary.Mu.Lock()
	for _, edge := range node.mesh.summary.Edges {
		edge.Pending = nil
	}
	node.mesh.summary.Mu.Unlock()

	if previous.Digest == current.Digest {
		t.Fatal("fixture: the view did not change")
	}
	node.mesh.summary.SetView(current, true, true)

	node.mesh.summary.Mu.Lock()
	defer node.mesh.summary.Mu.Unlock()
	if edge := node.mesh.summary.Edges[Key{Collection: "Demo000000000000", Location: "aa"}]; edge == nil || edge.Pending == nil && !edge.Stale {
		t.Fatal("leaf whose parent moved to J was not marked for re-push")
	}
	if edge := node.mesh.summary.Edges[Key{Collection: "Demo000000000000", Location: "aab"}]; edge != nil && (edge.Pending != nil || edge.Stale) {
		t.Fatal("leaf whose parent stayed local was re-pushed")
	}
}
