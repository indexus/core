package summary

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/wire"
)

func TestLocalSummaryReadinessCachesControlChanges(t *testing.T) {
	s := describedService(t, "cache", peerTransport{})
	initCluster(t, s)
	mark := New(s, 1, Hooks{})
	if !mark.LocalReady() {
		t.Fatal("genesis native region not ready")
	}
	cached := mark.readiness.Load()
	for i := 0; i < 20; i++ {
		if !mark.LocalReady() || mark.readiness.Load() != cached {
			t.Fatal("unchanged read rebuilt native/membership state")
		}
	}
	if _, err := s.Store.Write("ack", storage.Record{Collection: "Demo000000000000", Location: "0", ID: "one"}); err != nil {
		t.Fatal(err)
	}
	if !mark.LocalReady() || mark.readiness.Load() != cached {
		t.Fatal("ordinary record write invalidated control readiness")
	}
	state, _ := s.Store.CausalState()
	peer := describedService(t, "destination", peerTransport{})
	partition, err := s.Store.BeginCausalPiece(context.Background(), state.Permissions.Native[0].ID, peer.Self, tree.Region{Mask: tree.ID{128}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AbortCausal(partition.Cession.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.FinishAbortCausal(partition.Cession.ID); err != nil {
		t.Fatal(err)
	}
	if !mark.LocalReady() || mark.readiness.Load() == cached {
		t.Fatal("native change did not rebuild a still-complete cell")
	}
}

func TestIncompleteContributionPropagatesAndNewerCompleteRepairs(t *testing.T) {
	s := describedService(t, "parent", peerTransport{})
	initCluster(t, s)
	mark := New(s, 1, Hooks{})
	routing, _ := s.Store.CausalRouting()
	mark.SetView(routing.Current(), true, false)
	key := tree.Key{Collection: "Demo000000000000", Location: "00"}
	mark.incoming = map[tree.Key]summaryContribution{key: {incomplete: true, value: wire.NewAbelian(1, []float64{2})}}
	boundary := tree.Key{Collection: key.Collection, Location: "0"}
	mark.Mu.Lock()
	invalid, err := mark.wireLocked(boundary)
	mark.Mu.Unlock()
	if err != nil || !invalid.Incomplete {
		t.Fatal("incomplete child was exported as complete", invalid, err)
	}
	body, err := json.Marshal(mesh.Response{SummaryStatus: &mesh.SummaryStatus{Unavailable: true}, Contributions: []mesh.Contribution{invalid}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded mesh.Response
	if err := json.Unmarshal(body, &decoded); err != nil || !decoded.SummaryStatus.Unavailable || !decoded.Contributions[0].Incomplete {
		t.Fatal("JSON lost completeness", err)
	}
	// The local base remains ready, independent of the child's derived status:
	// recovery follows strict prefix ancestry rather than a readiness cycle.
	if !mark.LocalReady() || mark.ReadyFor(key.Collection, "@") {
		t.Fatal("local/derived readiness were conflated")
	}
	mark.Mu.Lock()
	fixed := mark.incoming[key]
	fixed.incomplete = false
	fixed.value = wire.NewAbelian(3, []float64{14})
	mark.incoming[key] = fixed
	valid, err := mark.wireLocked(boundary)
	mark.Mu.Unlock()
	if err != nil || valid.Incomplete || valid.Count != 3 || !newerContribution(contributionFromWire(invalid), contributionFromWire(valid)) {
		t.Fatal("complete newer leaf did not restore its ancestor", valid, err)
	}
}

type summaryResponseTransport struct{ response mesh.Response }

func (p *summaryResponseTransport) Exchange(_ context.Context, _ string, _ mesh.Request) (mesh.Response, error) {
	return p.response, nil
}

func TestSummaryPollFloorRejectsDelayedCompleteAcrossReceiverRestart(t *testing.T) {
	peers := peerTransport{}
	parent := describedService(t, "parent-floor", peers)
	path := filepath.Join(t.TempDir(), "child.db")
	store, err := storage.OpenWithOptions(path, "child-floor", "test", 1<<24, storage.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	holder, _ := store.Endpoint("http://child-floor")
	child := &mesh.Service{Store: store, Self: holder, VNode: tree.VNodeDescriptor{Name: "child-floor", ID: tree.ID{128}}, Peers: peers}
	peers[parent.Self.Host], peers[child.Self.Host] = parent, child
	t.Cleanup(parent.Close)
	initCluster(t, parent, child)
	if err := child.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := parent.Sync(context.Background(), []string{child.Self.Host}); err != nil {
		t.Fatal(err)
	}
	epoch, err := store.NextSummaryEpoch()
	if err != nil {
		t.Fatal(err)
	}
	child.Close()
	parent.Close()
	fake := &summaryResponseTransport{}
	facade := &mesh.Service{Store: parent.Store, Self: parent.Self, VNode: parent.VNode, Peers: fake}
	mark := New(facade, 1, Hooks{})
	routing, _ := parent.Store.CausalRouting()
	mark.SetView(routing.Current(), true, false)
	in := mesh.Contribution{Collection: "Demo000000000000", Location: tree.BASE64.CharAt(32), Holder: holder, Epoch: epoch, Revision: 10, Count: 1}
	if changed, _, err := mark.acceptChild(in); err != nil || !changed {
		t.Fatal("initial contribution refused", err)
	}
	fake.response = mesh.Response{SummaryStatus: &mesh.SummaryStatus{Holder: holder, Epoch: epoch, Revision: 20, Unavailable: true}}
	if err := mark.PollHost(context.Background(), holder.Host); err != nil {
		t.Fatal(err)
	}
	delayed := in
	delayed.Revision = 19 // prepared before the status, after the old value
	delayed.Count = 99
	if changed, _, err := mark.acceptChild(delayed); err != nil || changed {
		t.Fatal("delayed complete revived invalidated child", err)
	}
	// Open a new summary incarnation on the very same durable disk/endpoint.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.OpenWithOptions(path, "child-floor", "test", 1<<24, storage.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	nextEpoch, err := store.NextSummaryEpoch()
	if err != nil || nextEpoch <= epoch {
		t.Fatal("summary incarnation did not survive reopen", nextEpoch, err)
	}
	fake.response = mesh.Response{SummaryStatus: &mesh.SummaryStatus{Holder: holder, Epoch: nextEpoch, Revision: 1, Unavailable: true}}
	if err := mark.PollHost(context.Background(), holder.Host); err != nil {
		t.Fatal(err)
	}
	delayed.Revision = 1_000_000
	if changed, _, err := mark.acceptChild(delayed); err != nil || changed {
		t.Fatal("old incarnation defeated new completeness floor", err)
	}
	// A replayed old available status cannot lower the newer floor either.
	fake.response = mesh.Response{SummaryStatus: &mesh.SummaryStatus{Holder: holder, Epoch: epoch, Revision: 2_000_000}}
	if err := mark.PollHost(context.Background(), holder.Host); err != nil {
		t.Fatal(err)
	}
	if mark.peerFloor[holder.Host].Epoch != nextEpoch || !mark.peerUnavailable[holder.Host] {
		t.Fatal("old status replaced new unavailable incarnation")
	}
	fresh := in
	fresh.Epoch, fresh.Revision, fresh.Count = nextEpoch, 3, 3
	fake.response = mesh.Response{SummaryStatus: &mesh.SummaryStatus{Holder: holder, Epoch: nextEpoch, Revision: 2}, Contributions: []mesh.Contribution{fresh}}
	if err := mark.PollHost(context.Background(), holder.Host); err != nil {
		t.Fatal(err)
	}
	got := mark.incoming[tree.Key{Collection: in.Collection, Location: in.Location}]
	if got.incomplete || got.value.Count() != 3 || mark.peerUnavailable[holder.Host] {
		t.Fatal("fresh new-incarnation value did not repair incompleteness", got)
	}
}

// wireLocked is a boundary's contribution as it stands now.
func (t *Tree) wireLocked(key tree.Key) (mesh.Contribution, error) {
	return t.wireWithLocked(key, nil)
}
