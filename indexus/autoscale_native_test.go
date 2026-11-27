package indexus

import (
	"context"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestAutoscaleDoesNotWaitForWholeIncomingJoin(t *testing.T) {
	descriptors := []VNodeDescriptor{{ID: ID{255}}, {ID: ID{128}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	source, receiver := f.stores[0], f.stores[1]
	rights, err := source.Rights(tree.Region{})
	if err != nil || len(rights) == 0 {
		t.Fatal("missing source complement", err)
	}
	op, err := source.BeginCausal(ctx, rights[0].ID, f.services[1].Self, rights[0].Region)
	if err != nil {
		t.Fatal(err)
	}
	c, err := receiver.ReserveCausal(op.Cession)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.BindCausal(c.ID, c.Ticket); err != nil {
		t.Fatal(err)
	}
	op.Cession = c
	if _, err := receiver.PreloadCausal(op); err != nil {
		t.Fatal(err)
	}
	if err := source.ConfirmCausal(c.ID); err != nil {
		t.Fatal(err)
	}
	routing, err := receiver.CausalRouting()
	if err != nil || !routing.HasOpenTransfer() {
		t.Fatal("the receiving join must still be in progress", err)
	}
	if f.nodes[1].holdAutoscaleForTransfers() {
		t.Fatal("an unrelated incoming grain delayed scaling of installed native rights")
	}
	if err := source.AbortCausal(c.ID); err != nil {
		t.Fatal(err)
	}
	if err := receiver.CancelCausal(c); err != nil {
		t.Fatal(err)
	}
	if err := source.FinishAbortCausal(c.ID); err != nil {
		t.Fatal(err)
	}
	rights, err = receiver.Rights(tree.Region{})
	if err != nil || len(rights) != 1 {
		t.Fatal("unexpected receiver frontier", err)
	}
	pinned, err := receiver.BeginCausal(ctx, rights[0].ID, f.services[0].Self, rights[0].Region)
	if err != nil {
		t.Fatal(err)
	}
	if !f.nodes[1].holdAutoscaleForTransfers() {
		t.Fatal("scaling ignored its only native input already pinned by a transfer")
	}
	if err := receiver.AbortCausal(pinned.Cession.ID); err != nil {
		t.Fatal(err)
	}
	if f.nodes[1].holdAutoscaleForTransfers() {
		t.Fatal("cancelled cleanup debt blocked scaling")
	}
	if err := receiver.FinishAbortCausal(pinned.Cession.ID); err != nil {
		t.Fatal(err)
	}
}
