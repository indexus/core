package storage

import (
	"context"
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"
)

func confirmedAtEndpoint(t *testing.T, source, target *Store, input causal.Digest, region tree.Region, endpoint causal.Endpoint) CausalTransfer {
	t.Helper()
	op, e := source.BeginCausal(context.Background(), input, endpoint, region)
	if e != nil {
		t.Fatal(e)
	}
	c, e := target.ReserveCausal(op.Cession)
	if e != nil {
		t.Fatal(e)
	}
	if e = source.BindCausal(c.ID, c.Ticket); e != nil {
		t.Fatal(e)
	}
	op.Cession = c
	if _, e = target.PreloadCausal(op); e != nil {
		t.Fatal(e)
	}
	if e = source.ConfirmCausal(c.ID); e != nil {
		t.Fatal(e)
	}
	return op
}
func TestMovedEndpointRejectsNewOldEpochAdmissionButAcceptsPhysicalReturn(t *testing.T) {
	old, _, g := ticketStores(t)
	next := openTestData(t, t.TempDir()+"/physical", "A")
	defer next.Close()
	if e := next.InitCausalReceiver(tree.VNodeDescriptor{Name: "A", ID: tree.ID{1}}, g); e != nil {
		t.Fatal(e)
	}
	oldEndpoint, _ := old.Endpoint("A")
	future, _ := next.Endpoint("physical-A")
	future.Epoch = 1
	if e := old.BeginCausalMove(future); e != nil {
		t.Fatal(e)
	}
	first := confirmedAtEndpoint(t, old, next, g.Right().ID, tree.Region{}, future)
	ticketFinish(t, old, next, first.Cession.ID)
	member, e := old.PublishCausalMove()
	if e != nil {
		t.Fatal(e)
	}
	if e = next.MergeMembership(causal.MembershipDelta{Members: []causal.Member{member}}); e != nil {
		t.Fatal(e)
	}
	rights, e := next.Rights(tree.Region{})
	if e != nil {
		t.Fatal(e)
	}
	stale, e := next.BeginCausal(context.Background(), rights[0].ID, oldEndpoint, tree.Region{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = old.ReserveCausal(stale.Cession); !errors.Is(e, ErrReceiverClosed) {
		t.Fatal("moved endpoint reopened its retired epoch", e)
	}
	if e = next.AbortCausal(stale.Cession.ID); e != nil {
		t.Fatal(e)
	}
	if e = next.FinishAbortCausal(stale.Cession.ID); e != nil {
		t.Fatal(e)
	}
	routing, e := old.CausalRouting()
	if e != nil {
		t.Fatal(e)
	}
	if !routing.ReceiverDrained(oldEndpoint) {
		t.Fatal("published move did not close old endpoint")
	}
	returned := oldEndpoint
	returned.Epoch = 2
	if routing.ReceiverDrained(returned) {
		t.Fatal("future physical return mistaken for drained epoch")
	}
	if e = next.BeginCausalMove(returned); e != nil {
		t.Fatal(e)
	}
	back := confirmedAtEndpoint(t, next, old, rights[0].ID, tree.Region{}, returned)
	ticketFinish(t, next, old, back.Cession.ID)
	// Still no epoch-2 membership publication. Installed authority alone permits ACK.
	if _, e = old.Write("returned", Record{Collection: "Demo000000000000", Location: "aa", ID: "returned"}); e != nil {
		t.Fatal("future physical install was gated by membership", e)
	}
}
func TestMovePrunesTentativeSlotBeforeClosingIntake(t *testing.T) {
	source, donor, g := ticketStores(t)
	half := tree.Region{Mask: tree.ID{128}}
	first := ticketAdmission(t, source, donor, g.Right().ID, half)
	ticketFinish(t, source, donor, first.Cession.ID)
	rights, e := donor.Rights(half)
	if e != nil {
		t.Fatal(e)
	}
	endpoint, _ := source.Endpoint("A")
	pending, e := donor.BeginCausal(context.Background(), rights[0].ID, endpoint, half)
	if e != nil {
		t.Fatal(e)
	}
	c, e := source.ReserveCausal(pending.Cession)
	if e != nil {
		t.Fatal(e)
	}
	if e = donor.BindCausal(c.ID, c.Ticket); e != nil {
		t.Fatal(e)
	}
	pending.Cession = c
	physical := causal.Endpoint{Owner: causal.Owner{VNode: "A", Storage: "next-disk"}, Host: "next-host", Epoch: 1}
	if e = source.BeginCausalMove(physical); e != nil {
		t.Fatal("tentative slot pinned physical movement", e)
	}
	state, e := source.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := state.Transfers[c.ID]; ok {
		t.Fatal("tentative header survived intake closure")
	}
	if _, e = source.PreloadCausal(pending); !errors.Is(e, ErrReceiverClosed) {
		t.Fatal("pruned ticket confirmed after intake closure", e)
	}
	if e = donor.AbortCausal(c.ID); e != nil {
		t.Fatal(e)
	}
	if e = source.CancelCausal(c); e != nil {
		t.Fatal(e)
	}
	if e = donor.FinishAbortCausal(c.ID); e != nil {
		t.Fatal(e)
	}
}

func TestColdDrainPrunesTentativeSlotBeforeClosingIntake(t *testing.T) {
	root, donor, g := ticketStores(t)
	half := tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}
	first := ticketAdmission(t, root, donor, g.Right().ID, half)
	ticketFinish(t, root, donor, first.Cession.ID)
	endpoint, _ := donor.Endpoint("B")
	record, e := donor.PublishActivation(tree.VNodeDescriptor{Name: "B", ID: tree.ID{129}}, endpoint)
	if e != nil {
		t.Fatal(e)
	}
	if e = root.MergeMembership(causal.MembershipDelta{Members: []causal.Member{record}}); e != nil {
		t.Fatal(e)
	}
	rights, e := root.Rights(tree.Region{Mask: tree.ID{128}})
	if e != nil || len(rights) != 1 {
		t.Fatal(e)
	}
	pending, e := root.BeginCausal(context.Background(), rights[0].ID, endpoint, rights[0].Region)
	if e != nil {
		t.Fatal(e)
	}
	c, e := donor.ReserveCausal(pending.Cession)
	if e != nil {
		t.Fatal(e)
	}
	pending.Cession = c
	target, _ := root.Endpoint("A")
	if _, e = donor.BeginColdDrain("cold-prune", target, ColdLimits{}); e != nil {
		t.Fatal("tentative slot pinned retirement", e)
	}
	state, e := donor.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := state.Transfers[c.ID]; ok {
		t.Fatal("retirement left tentative admission open")
	}
	if _, e = donor.PreloadCausal(pending); !errors.Is(e, ErrReceiverClosed) {
		t.Fatal("tentative preload confirmed after cold intake closure", e)
	}
}

func TestPhysicalMoveEndpointEpochCannotWrap(t *testing.T) {
	source, _, _ := ticketStores(t)
	endpoint, _ := source.Endpoint("A")
	endpoint.Epoch = ^uint64(0)
	if _, e := source.PublishActivation(tree.VNodeDescriptor{Name: "A", ID: tree.ID{1}}, endpoint); e != nil {
		t.Fatal(e)
	}
	target := causal.Endpoint{Owner: causal.Owner{VNode: "A", Storage: "different-disk"}, Host: "other", Epoch: 0}
	if e := source.BeginCausalMove(target); !errors.Is(e, ErrConflict) {
		t.Fatal("physical epoch wrapped", e)
	}
	state, e := source.CausalState()
	if e != nil || state.Moving != nil {
		t.Fatal("overflow closed intake", e)
	}
}
