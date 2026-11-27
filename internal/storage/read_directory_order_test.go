package storage

import (
	"context"
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"
)

func TestConfirmedDirectorySurvivesCancelAndDelayedOlderPreload(t *testing.T) {
	s, r, g := ticketStores(t)
	c := openTestData(t, t.TempDir()+"/C", "C")
	defer c.Close()
	if e := c.InitCausal(tree.VNodeDescriptor{Name: "A", ID: tree.ID{1}}, g); e != nil {
		t.Fatal(e)
	}
	first := ticketAdmission(t, s, r, g.Right().ID, tree.Region{})
	ticketFinish(t, s, r, first.Cession.ID)
	rights, _ := r.Rights(tree.Region{})
	target, _ := s.Endpoint("A")
	old, e := r.BeginCausal(context.Background(), rights[0].ID, target, tree.Region{})
	if e != nil {
		t.Fatal(e)
	}
	old.Cession, e = s.ReserveCausal(old.Cession)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.BindCausal(old.Cession.ID, old.Cession.Ticket); e != nil {
		t.Fatal(e)
	}
	// The source aborts, but both its old preload and cancellation are delayed.
	if e = r.AbortCausal(old.Cession.ID); e != nil {
		t.Fatal(e)
	}
	toC := ticketAdmission(t, r, c, rights[0].ID, tree.Region{})
	ticketFinish(t, r, c, toC.Cession.ID)
	cRights, _ := c.Rights(tree.Region{})
	newer := ticketAdmission(t, c, s, cRights[0].ID, tree.Region{})
	assertCurrent := func() {
		t.Helper()
		if e := s.view(func(tx *Tx) error {
			directory, e := readDelegations(tx)
			if e != nil {
				return e
			}
			if len(directory) != 1 || directory[0].Holder.Owner != newer.Cession.Source.Owner || directory[0].Stamp != newer.Cession.Ticket {
				t.Fatalf("current directory regressed: %+v", directory)
			}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
	}
	assertCurrent()
	if _, e = s.PreloadCausal(old); e != nil {
		t.Fatal("delayed warm packet may confirm its old bound slot, not its ownership", e)
	}
	assertCurrent()
	if e = s.CancelCausal(old.Cession); e != nil {
		t.Fatal(e)
	}
	if e = r.FinishAbortCausal(old.Cession.ID); e != nil {
		t.Fatal(e)
	}
	if e = c.AbortCausal(newer.Cession.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.CancelCausal(newer.Cession); e != nil {
		t.Fatal(e)
	}
	if e = c.FinishAbortCausal(newer.Cession.ID); e != nil {
		t.Fatal(e)
	}
	assertCurrent() // C kept the real native input when it aborted.
	if _, e = s.EmitCausal(first.Cession.ID); e != nil {
		t.Fatal(e)
	}
	assertCurrent()
	rows, page, e := s.CausalInventoryPage(context.Background(), nil, InventoryPageRequest{})
	if e != nil {
		t.Fatal(e)
	}
	_ = rows
	if len(page.Referrals) != 1 || page.Referrals[0].Endpoint.Owner != newer.Cession.Source.Owner {
		t.Fatalf("historical operation resurrected old regional dependency: %+v", page.Referrals)
	}
}

func TestReplayOfOldEmitCannotRestoreDirectoryInsideReturnedNative(t *testing.T) {
	source, target, g := ticketStores(t)
	first := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	ticketFinish(t, source, target, first.Cession.ID)
	rights, _ := target.Rights(tree.Region{})
	back := ticketAdmission(t, target, source, rights[0].ID, tree.Region{})
	ticketFinish(t, target, source, back.Cession.ID)
	before, revision := source.LastTransferStep(), source.LocalTransferRevision()
	if _, e := source.EmitCausal(first.Cession.ID); e != nil {
		t.Fatal(e)
	}
	directory, e := source.ReadDirectory()
	if e != nil || len(directory) != 0 {
		t.Fatal("replayed old emit reintroduced a consumed successor", directory, e)
	}
	if !source.LastTransferStep().Equal(before) || source.LocalTransferRevision() != revision {
		t.Fatal("old emit replay was not a pure ACK")
	}
}
