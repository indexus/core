package storage

import (
	"context"
	"errors"
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"
)

func TestCoalesceLocalRightsAfterReturnPreservesACKsAndClosedTickets(t *testing.T) {
	source, target, g := ticketStores(t)
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "original", Metrics: []float64{1}}
	z, _ := recordZone(record)
	region := tree.Region{Mask: tree.ID{128}, Value: tree.ID{z.ID[0] & 128}}
	if _, e := source.Write("original", record); e != nil {
		t.Fatal(e)
	}
	out := ticketAdmission(t, source, target, g.Right().ID, region)
	first := ticketFinish(t, source, target, out.Cession.ID)
	rights, e := target.Rights(region)
	if e != nil || len(rights) != 1 {
		t.Fatal(e)
	}
	back := ticketAdmission(t, target, source, rights[0].ID, region)
	last := ticketFinish(t, target, source, back.Cession.ID)
	before, e := source.CausalState()
	if e != nil || len(before.Permissions.Native) != 2 {
		t.Fatal("fixture needs sibling frontier", e)
	}
	if e = source.CoalesceLocalRights(); e != nil {
		t.Fatal(e)
	}
	after, e := source.CausalState()
	if e != nil || len(after.Permissions.Native) != 1 || after.Permissions.Native[0].Region != (tree.Region{}) || after.Permissions.Sequence <= before.Permissions.Sequence {
		t.Fatal("frontier not compacted with fresh sequence", e)
	}
	if e = source.CollectTransferred(first.ID); e != nil {
		t.Fatal(e)
	}
	if e = target.CollectTransferred(first.ID); e != nil {
		t.Fatal(e)
	}
	if e = source.CollectTransferred(last.ID); e != nil {
		t.Fatal(e)
	}
	record.ID = "after-coalesce"
	if _, e = source.Write("after-coalesce", record); e != nil {
		t.Fatal(e)
	}
	if e = source.InstallCausal(last); e != nil {
		t.Fatal("closed return after coalesce", e)
	}
	if e = target.InstallCausal(first); e != nil {
		t.Fatal("closed outgoing after return", e)
	}
	snap, e := source.Snapshot(z)
	if e != nil || len(snap.Records) != 2 {
		t.Fatal("replay or old collector lost ACK", e)
	}
	state, e := target.CausalState()
	if e != nil || len(state.Permissions.Native) != 0 {
		t.Fatal("closed ticket resurrected old owner", e)
	}
}
func TestCoalesceLocalRightsRespectsPinnedSourceInputs(t *testing.T) {
	source, target, g := ticketStores(t)
	region := tree.Region{Mask: tree.ID{128}}
	first := ticketAdmission(t, source, target, g.Right().ID, region)
	ticketFinish(t, source, target, first.Cession.ID)
	rights, _ := target.Rights(region)
	back := ticketAdmission(t, target, source, rights[0].ID, region)
	ticketFinish(t, target, source, back.Cession.ID)
	current, _ := source.Rights(region)
	targetEndpoint, _ := target.Endpoint("B")
	op, e := source.BeginCausal(context.Background(), current[0].ID, targetEndpoint, region)
	if e != nil {
		t.Fatal(e)
	}
	if e = source.CoalesceLocalRights(); e != nil {
		t.Fatal(e)
	}
	state, _ := source.CausalState()
	if len(state.Permissions.Native) != 2 {
		t.Fatal("coalesced a pinned source input")
	}
	if e = source.AbortCausal(op.Cession.ID); e != nil {
		t.Fatal(e)
	}
	if e = source.FinishAbortCausal(op.Cession.ID); e != nil {
		t.Fatal(e)
	}
	if e = source.CoalesceLocalRights(); e != nil {
		t.Fatal(e)
	}
	state, _ = source.CausalState()
	if len(state.Permissions.Native) != 1 {
		t.Fatal("cancelled source still pins input")
	}
}
func TestPhysicalReceiverCannotRunSourcePointAPIs(t *testing.T) {
	source, _, g := ticketStores(t)
	receiver := openTestData(t, t.TempDir()+"/same-vnode", "A")
	defer receiver.Close()
	if e := receiver.InitCausalReceiver(tree.VNodeDescriptor{Name: "A", ID: tree.ID{1}}, g); e != nil {
		t.Fatal(e)
	}
	ep, _ := receiver.Endpoint("physical-A")
	ep.Epoch = 1
	op, e := source.BeginCausal(context.Background(), g.Right().ID, ep, tree.Region{})
	if e != nil {
		t.Fatal(e)
	}
	c, e := receiver.ReserveCausal(op.Cession)
	if e != nil {
		t.Fatal(e)
	}
	op.Cession = c
	if _, e = receiver.PreloadCausal(op); e != nil {
		t.Fatal(e)
	}
	z, _ := tree.NewZoneKey("Demo000000000000", "aa")
	if _, e = receiver.FreezeZones(c.ID, []tree.ZoneKey{z}); !errors.Is(e, ErrNotOwner) {
		t.Fatal("same vnode receiver acted as source", e)
	}
	if _, e = receiver.EmitZoneTransfers(c.ID, []tree.ZoneKey{z}); !errors.Is(e, ErrNotOwner) {
		t.Fatal("same vnode receiver emitted source proof", e)
	}
}
func TestCancelledDiscoveryLeavesNoHistoricalRows(t *testing.T) {
	source, target, g := ticketStores(t)
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "live"}
	if _, e := source.Write("live", record); e != nil {
		t.Fatal(e)
	}
	for range 5 {
		op := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
		if e := source.AbortCausal(op.Cession.ID); e != nil {
			t.Fatal(e)
		}
		if e := target.CancelCausal(op.Cession); e != nil {
			t.Fatal(e)
		}
		if e := source.FinishAbortCausal(op.Cession.ID); e != nil {
			t.Fatal(e)
		}
		for _, store := range []*Store{source, target} {
			if e := store.view(func(tx *Tx) error {
				if tx.any(zoneStatesRows(op.Cession.ID)) || tx.value(zoneStateVerPath(op.Cession.ID)) != nil || tx.any(stagingRows(op.Cession.ID.String())) || tx.any(transferRows(op.Cession.ID)) {
					t.Fatal("cancel retained historical transfer rows")
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
		}
	}
}
