package summary

import (
	"context"
	"testing"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestRemoteCessionDoesNotRefoldUnchangedLocalData(t *testing.T) {
	peers := peerTransport{}
	observer := describedService(t, "observer", peers)
	donor := describedService(t, "donor", peers)
	donor.VNode.ID = tree.ID{81, 1}
	target := describedService(t, "target", peers)
	target.VNode.ID = tree.ID{161, 1}
	for _, s := range []*mesh.Service{observer, donor, target} {
		peers[s.Self.Host] = s
		t.Cleanup(s.Close)
	}
	initCluster(t, observer, donor, target)
	ctx := context.Background()
	if err := donor.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	mark := New(observer, 1, Hooks{Quiet: func() bool { return false }})
	before, err := observer.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	mark.SetView(before.Current(), true, false)
	if _, err := mark.Fold(ctx, true); err != nil {
		t.Fatal(err)
	}
	revision := observer.Store.LocalTransferRevision()
	// An operation on another disk is local control, not gossiped authority.
	state, err := donor.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Permissions.Native) == 0 {
		t.Fatal("missing donor right")
	}
	right := state.Permissions.Native[0]
	if _, err := donor.Store.BeginCausal(ctx, right.ID, target.Self, right.Region); err != nil {
		t.Fatal(err)
	}
	if err := observer.Sync(ctx, []string{donor.Self.Host}); err != nil {
		t.Fatal(err)
	}
	after, err := observer.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if before.RoutingDigest() != after.RoutingDigest() || observer.Store.LocalTransferRevision() != revision {
		t.Fatal("unrelated graph change affected local routing/transfer projection")
	}
	if ran, err := mark.Fold(ctx, false); err != nil || ran {
		t.Fatalf("unrelated remote fact rescanned local data: ran=%v err=%v", ran, err)
	}
	// A local transfer step must invalidate that same repair projection.
	own, err := observer.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Permissions.Native) == 0 {
		t.Fatal("missing observer right")
	}
	right = own.Permissions.Native[0]
	if _, err := observer.Store.BeginCausal(ctx, right.ID, target.Self, right.Region); err != nil {
		t.Fatal(err)
	}
	if ran, err := mark.Fold(ctx, false); err != nil || !ran {
		t.Fatalf("local step did not invalidate fold: ran=%v err=%v", ran, err)
	}
}
