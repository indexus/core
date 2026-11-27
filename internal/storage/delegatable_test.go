package storage

import (
	"context"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestInstalledGrainCanDelegateWhileAnotherGrainIsIncoming(t *testing.T) {
	a, b, genesis := ticketStores(t)
	check := func(s *Store, want bool) {
		t.Helper()
		got, err := s.HasDelegatableRight()
		if err != nil || got != want {
			t.Fatalf("%s delegatable=%t want=%t err=%v", s.node, got, want, err)
		}
	}
	left := tree.Region{Mask: tree.ID{128}}
	first := ticketAdmission(t, a, b, genesis.Right().ID, left)
	check(a, false) // its input is pinned until this first cession
	check(b, false) // provisional points alone cannot be subdelegated
	ticketFinish(t, a, b, first.Cession.ID)
	state, err := a.CausalState()
	if err != nil || len(state.Permissions.Native) != 1 {
		t.Fatal(err)
	}
	second := ticketAdmission(t, a, b, state.Permissions.Native[0].ID, state.Permissions.Native[0].Region)
	check(a, false)
	check(b, true) // its installed first grain is independent of the incoming one
	state, err = b.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	target, _ := a.Endpoint(a.node)
	pinned, err := b.BeginCausal(context.Background(), state.Permissions.Native[0].ID, target, left)
	if err != nil {
		t.Fatal(err)
	}
	check(b, false)
	if err := b.AbortCausal(pinned.Cession.ID); err != nil {
		t.Fatal(err)
	}
	check(b, true) // cleanup debt does not pin a native right
	if err := b.FinishAbortCausal(pinned.Cession.ID); err != nil {
		t.Fatal(err)
	}
	ticketFinish(t, a, b, second.Cession.ID)
	endpoint, _ := b.Endpoint(b.node)
	if _, err := b.PublishActivation(tree.VNodeDescriptor{Name: b.node, ID: tree.ID{128}}, endpoint); err != nil {
		t.Fatal(err)
	}
	future := causal.Endpoint{Owner: causal.Owner{VNode: b.node, Storage: "next-disk"}, Host: "next-host", Epoch: endpoint.Epoch + 1}
	if err := b.BeginCausalMove(future); err != nil {
		t.Fatal(err)
	}
	check(b, false) // a physical departure still closes local admission
}
