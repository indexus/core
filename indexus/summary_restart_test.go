package indexus

import (
	"context"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

func TestSummaryChildRuntimeRestartConvergesWithRetainedParent(t *testing.T) {
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
	const collection = "0000000000000001"
	root, _ := NewZoneKey(collection, "@")
	rootOwner, _ := f.nodes[0].mesh.summary.View().OwnerName(root.ID)
	var parent, child *Node
	for _, n := range f.nodes {
		if n.Name() == rootOwner {
			parent = n
		} else {
			child = n
		}
	}
	location := ""
	for _, candidate := range ChildCandidates(BASE64, "@") {
		z, _ := NewZoneKey(collection, candidate)
		owner, _ := child.mesh.summary.View().OwnerName(z.ID)
		if owner == child.Name() {
			location = candidate
			break
		}
	}
	if location == "" {
		t.Fatal("missing cross-node edge")
	}
	for i := 0; i < 10; i++ {
		record := storage.Record{Collection: collection, Location: location, ID: fmt.Sprint(i), Metrics: []float64{1}}
		if _, err := child.MeshData(ctx, mesh.Request{Kind: "write", RequestID: fmt.Sprint(i), Record: &record}); err != nil {
			t.Fatal(err)
		}
	}
	oldWire := child.mesh.summary.Export(collection, []string{location}, "")
	if len(oldWire) != 1 {
		t.Fatalf("old wire count=%d", len(oldWire))
	}
	if err := parent.mesh.summary.AcceptWire(oldWire[0]); err != nil {
		t.Fatal(err)
	}
	if got := parent.mesh.summary.Owned(collection, "@").Count(); got != 10 {
		t.Fatalf("old parent count=%d", got)
	}
	oldService := child.mesh.service
	fresh, err := NewNode(child.settings, testNewContact, nil)
	if err != nil {
		t.Fatal(err)
	}
	freshService := &mesh.Service{Store: oldService.Store, Self: oldService.Self, VNode: oldService.VNode, Peers: f.net}
	if err = fresh.EnableCausalCluster(freshService); err != nil {
		t.Fatal(err)
	}
	f.net.nodes[freshService.Self.Host] = fresh
	defer func() {
		for _, n := range []*Node{parent, child, fresh} {
			n.mesh.summary.Mu.Lock()
			n.mesh.summary.StopFlushLocked()
			n.mesh.summary.Mu.Unlock()
		}
	}()
	if !fresh.mesh.summary.Ensure(context.Background()) {
		t.Fatal("fresh inventory fold failed")
	}
	record := storage.Record{Collection: collection, Location: location, ID: "after-restart", Metrics: []float64{1}}
	if _, err = fresh.MeshData(ctx, mesh.Request{Kind: "write", RequestID: "after-restart", Record: &record}); err != nil {
		t.Fatal(err)
	}
	freshWire := fresh.mesh.summary.Export(collection, []string{location}, "")
	if len(freshWire) != 1 {
		t.Fatal("missing fresh summary")
	}
	if oldWire[0].Holder != freshWire[0].Holder || oldWire[0].Generation != freshWire[0].Generation {
		t.Fatal("restart changed authority identity; test requires same holder/generation")
	}
	for i := 0; i < 3; i++ {
		if err = parent.mesh.summary.PollRemoteHosts(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	inventory, err := parent.mesh.service.QueryPrefix(ctx, collection, location)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, s := range inventory {
		rows += len(s.Records)
	}
	parentCount := parent.mesh.summary.Owned(collection, "@").Count()
	t.Logf("old summary rev=%d gen=%d count=%d; restarted summary rev=%d gen=%d count=%d; after3 boundary discovery polls parent=%d exactInventory=%d sameHolder=%v", oldWire[0].Revision, oldWire[0].Generation, oldWire[0].Count, freshWire[0].Revision, freshWire[0].Generation, freshWire[0].Count, parentCount, rows, oldWire[0].Holder == freshWire[0].Holder)
	if rows != 11 || freshWire[0].Count != 11 || parentCount != 11 {
		t.Fatal("parent failed to converge after child runtime restart")
	}
	if freshWire[0].Epoch <= oldWire[0].Epoch || freshWire[0].Revision >= oldWire[0].Revision {
		t.Fatal("test must exercise a new epoch with a lower volatile revision")
	}
	// Old incarnation messages remain stale even if their revision is higher.
	if err = parent.mesh.summary.AcceptWire(oldWire[0]); err != nil {
		t.Fatal(err)
	}
	if err = parent.mesh.summary.AcceptWire(freshWire[0]); err != nil {
		t.Fatal(err)
	}
	if got := parent.mesh.summary.Owned(collection, "@").Count(); got != 11 {
		t.Fatalf("old epoch or duplicate changed parent: %d", got)
	}
}
func TestSummaryDelayedOldPhysicalContributionCannotRollbackAfterMove(t *testing.T) {
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
	if !parent.mesh.summary.OwnsHere(collection, "@") {
		t.Fatal("fixture root not on parent")
	}
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
	if err := parent.mesh.summary.AcceptWire(oldWire); err != nil {
		t.Fatal(err)
	}
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
	record.ID = "after"
	record.Metrics = []float64{1}
	if _, err := newChild.MeshData(ctx, mesh.Request{Kind: "write", RequestID: "after", Record: &record}); err != nil {
		t.Fatal(err)
	}
	if _, err := newChild.mesh.summary.Fold(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	newWire := newChild.mesh.summary.Export(collection, []string{location}, "")[0]
	if oldWire.Holder.VNode != newWire.Holder.VNode || oldWire.Holder.Storage == newWire.Holder.Storage {
		t.Fatal("test requires a real MOVE preserving vnode and replacing storage")
	}
	if err := parent.mesh.summary.AcceptWire(newWire); err != nil {
		t.Fatal(err)
	}
	if got := parent.mesh.summary.Owned(collection, "@").Count(); got != 2 {
		t.Fatalf("new placement count=%d oldwire=%+v newwire=%+v", got, oldWire, newWire)
	}
	snapshot, err := parent.mesh.service.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	placement, err := snapshot.Placement(newWire.Holder.VNode)
	if err != nil {
		t.Fatal(err)
	}
	if placement.Storage != newWire.Holder.Storage {
		t.Fatal("parent does not know current storage")
	}
	// Deliver an authentic old contribution after the MOVE through the service
	// and Node MeshData dispatcher, with valid current metadata piggybacked.
	if _, err := f.services[1].Exchange(ctx, f.services[0].Self.Host, mesh.Request{Kind: mesh.KindSummaryContribute, Contributions: []mesh.Contribution{oldWire}}); err != nil {
		t.Fatal(err)
	}
	got := parent.mesh.summary.Owned(collection, "@").Count()
	t.Logf("current placement storage=%s; old wire storage=%s new wire storage=%s; correct count=2 parent after delayed old message=%d; revision old/new=%d/%d", placement.Storage, oldWire.Holder.Storage, newWire.Holder.Storage, got, oldWire.Revision, newWire.Revision)
	if got != 2 {
		t.Fatal("old physical holder rolled back the current placement summary")
	}
	if _, err := f.services[2].Exchange(ctx, f.services[0].Self.Host, mesh.Request{Kind: mesh.KindSummaryContribute, Contributions: []mesh.Contribution{newWire}}); err != nil {
		t.Fatal(err)
	}
	if parent.mesh.summary.Owned(collection, "@").Count() != 2 {
		t.Fatal("duplicate contribution changed parent")
	}
}

func TestSummaryHolderValidationUsesCertifiedPhysicalPlacement(t *testing.T) {
	t.Parallel()
	node := newNodeOn(t, 64)
	current := node.mesh.service.Self
	if !node.mesh.summary.ChildHolderCurrent("Demo000000000000", "aa", current) {
		t.Fatal("current certified holder rejected")
	}
	for _, field := range []string{"host", "storage", "vnode"} {
		t.Run(field, func(t *testing.T) {
			stale := current
			switch field {
			case "host":
				stale.Host += "-old"
			case "storage":
				stale.Storage += "-old"
			case "vnode":
				stale.VNode += "-old"
			}
			if node.mesh.summary.ChildHolderCurrent("Demo000000000000", "aa", stale) {
				t.Fatalf("non-current %s accepted", field)
			}
		})
	}
}
