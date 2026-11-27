package storage

import (
	"context"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"
)

func TestRemoteMembershipDoesNotCountAsLocalTransferProgress(t *testing.T) {
	source, target, g := ticketStores(t)
	half := tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}
	first := ticketAdmission(t, source, target, g.Right().ID, half)
	ticketFinish(t, source, target, first.Cession.ID)
	endpoint, _ := target.Endpoint("B")
	vnode := tree.VNodeDescriptor{Name: "B", ID: tree.ID{129}}
	member, e := target.PublishActivation(vnode, endpoint)
	if e != nil {
		t.Fatal(e)
	}
	if e = source.MergeMembership(causal.MembershipDelta{Members: []causal.Member{member}}); e != nil {
		t.Fatal(e)
	}
	before, revision := source.LastTransferStep(), source.LocalTransferRevision()
	endpoint.Host = "B-new"
	endpoint.Epoch++
	member, e = target.PublishActivation(vnode, endpoint)
	if e != nil {
		t.Fatal(e)
	}
	if e = source.MergeMembership(causal.MembershipDelta{Members: []causal.Member{member}}); e != nil {
		t.Fatal(e)
	}
	if !source.LastTransferStep().Equal(before) || source.LocalTransferRevision() != revision {
		t.Fatal("remote contact advancement disguised a local transfer step")
	}
	rights, e := source.Rights(tree.Region{})
	if e != nil {
		t.Fatal(e)
	}
	op, e := source.BeginCausal(context.Background(), rights[0].ID, endpoint, rights[0].Region)
	if e != nil {
		t.Fatal(e)
	}
	if !source.LastTransferStep().After(before) || source.LocalTransferRevision() <= revision {
		t.Fatal("actual Begin did not count as local progress")
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
	before, revision = source.LastTransferStep(), source.LocalTransferRevision()
	if e = source.ConfirmCausal(c.ID); e != nil {
		t.Fatal(e)
	}
	if !source.LastTransferStep().After(before) || source.LocalTransferRevision() <= revision {
		t.Fatal("actual confirmation did not count as local progress")
	}
}
