package storage

import (
	"context"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"testing"
)

// ticketAdmission uses a real durable receiver. No test synthesizes confirmation.
func ticketAdmission(t *testing.T, source, target *Store, input causal.Digest, region tree.Region) CausalTransfer {
	t.Helper()
	targetEndpoint, e := target.Endpoint(target.node)
	if e != nil {
		t.Fatal(e)
	}
	op, e := source.BeginCausal(context.Background(), input, targetEndpoint, region)
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
	op, _, e = source.LoadCausalTransfer(c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = target.PreloadCausal(op); e != nil {
		t.Fatal(e)
	}
	if e = source.ConfirmCausal(c.ID); e != nil {
		t.Fatal(e)
	}
	op, _, e = source.LoadCausalTransfer(c.ID)
	if e != nil {
		t.Fatal(e)
	}
	return op
}
func ticketFinish(t *testing.T, source, target *Store, id causal.TransferID) causal.Cession {
	t.Helper()
	states, e := source.TransferZones(id)
	if e != nil {
		t.Fatal(e)
	}
	// Descendants first is the authority rule, independent of heat scheduling.
	for i := 0; i < len(states); i++ {
		for j := i + 1; j < len(states); j++ {
			if len(states[j].Zone.Location) > len(states[i].Zone.Location) {
				states[i], states[j] = states[j], states[i]
			}
		}
	}
	for _, st := range states {
		if st.Acked {
			continue
		}
		snaps, e := source.FreezeZones(id, []tree.ZoneKey{st.Zone})
		if e != nil {
			t.Fatal(e)
		}
		if e = target.StageZones(id, source.node, snaps); e != nil {
			t.Fatal(e)
		}
		proofs, e := source.EmitZoneTransfers(id, []tree.ZoneKey{st.Zone})
		if e != nil {
			t.Fatal(e)
		}
		if e = target.InstallZones(id, proofs); e != nil {
			t.Fatal(e)
		}
		if e = source.AckZoneInstalls(id, []tree.ZoneKey{st.Zone}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = source.ForceFreezeRegion(id); e != nil {
		t.Fatal(e)
	}
	offer, e := source.SealCausalRegion(id)
	if e != nil {
		t.Fatal(e)
	}
	if e = target.StageCausal(offer); e != nil {
		t.Fatal(e)
	}
	c, e := source.EmitCausal(id)
	if e != nil {
		t.Fatal(e)
	}
	if e = target.InstallCausal(c); e != nil {
		t.Fatal(e)
	}
	if e = source.CompleteCausal(id); e != nil {
		t.Fatal(e)
	}
	return c
}
func ticketStores(t *testing.T) (*Store, *Store, causal.Genesis) {
	t.Helper()
	dir := t.TempDir()
	a := openTestData(t, filepath.Join(dir, "a"), "A")
	b := openTestData(t, filepath.Join(dir, "b"), "B")
	t.Cleanup(func() { a.Close(); b.Close() })
	ep, e := a.Endpoint("A")
	if e != nil {
		t.Fatalf("uninitialized endpoint %v", e)
	}
	identity, e := a.Identity()
	if e != nil {
		t.Fatal(e)
	}
	ep = causal.Endpoint{Owner: causal.Owner{VNode: "A", Storage: identity}, Host: "A"}
	g := causal.Genesis{Cluster: "test", Holder: ep}
	for _, s := range []*Store{a, b} {
		if e = s.InitCausal(tree.VNodeDescriptor{Name: "A", ID: tree.ID{1}}, g); e != nil {
			t.Fatal(e)
		}
	}
	return a, b, g
}
func TestLocalTicketsGranularEarlyACKAndReplay(t *testing.T) {
	a, b, g := ticketStores(t)
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "before", Metrics: []float64{1}}
	if _, e := a.Write("before", record); e != nil {
		t.Fatal(e)
	}
	op := ticketAdmission(t, a, b, g.Right().ID, tree.Region{})
	id := op.Cession.ID
	z, _ := recordZone(record)
	snaps, e := a.FreezeZones(id, []tree.ZoneKey{z})
	if e != nil {
		t.Fatal(e)
	}
	if e = b.StageZones(id, a.node, snaps); e != nil {
		t.Fatal(e)
	}
	proofs, e := a.EmitZoneTransfers(id, []tree.ZoneKey{z})
	if e != nil {
		t.Fatal(e)
	}
	if e = b.InstallZones(id, proofs); e != nil {
		t.Fatal(e)
	}
	record.ID = "early"
	if _, e = b.Write("early", record); e != nil {
		t.Fatal("early receiver ACK", e)
	}
	if _, e = a.Write("illegal", record); e == nil {
		t.Fatal("two writers")
	}
	if e = a.AbortCausal(id); !errors.Is(e, ErrNotReady) {
		t.Fatal("cancel after first fence", e)
	}
	if e = a.AckZoneInstalls(id, []tree.ZoneKey{z}); e != nil {
		t.Fatal(e)
	}
	c := ticketFinish(t, a, b, id)
	if e = b.InstallCausal(c); e != nil {
		t.Fatal("lost install reply", e)
	}
	if e = b.CollectTransferred(id); e != nil {
		t.Fatal(e)
	}
	if e = b.InstallCausal(c); e != nil {
		t.Fatal("compacted replay", e)
	}
	snap, e := b.Snapshot(z)
	if e != nil || len(snap.Records) != 2 {
		t.Fatal("final native install lost early ACK", e, len(snap.Records))
	}
	if e = a.CollectTransferred(id); e != nil {
		t.Fatal(e)
	}
}
func TestLocalTicketsNoFenceBeforeConfirmation(t *testing.T) {
	a, b, g := ticketStores(t)
	ep, _ := b.Endpoint("B")
	op, e := a.BeginCausal(context.Background(), g.Right().ID, ep, tree.Region{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.ForceFreezeRegion(op.Cession.ID); !errors.Is(e, ErrNotReady) {
		t.Fatal("unconfirmed fence", e)
	}
	c, e := b.ReserveCausal(op.Cession)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.BindCausal(c.ID, c.Ticket); e != nil {
		t.Fatal(e)
	}
	if _, e = a.ForceFreezeRegion(c.ID); !errors.Is(e, ErrNotReady) {
		t.Fatal("reserved ticket authorizes fence", e)
	}
}
func TestLocalTicketsReturnOldCollectorAndAdmissionReplay(t *testing.T) {
	a, b, g := ticketStores(t)
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "ack", Metrics: []float64{1}}
	if _, e := a.Write("ack", record); e != nil {
		t.Fatal(e)
	}
	first := ticketAdmission(t, a, b, g.Right().ID, tree.Region{})
	old := ticketFinish(t, a, b, first.Cession.ID)
	rights, e := b.Rights(tree.Region{})
	if e != nil {
		t.Fatal(e)
	}
	back := ticketAdmission(t, b, a, rights[0].ID, tree.Region{})
	ticketFinish(t, b, a, back.Cession.ID)
	if e = a.CollectTransferred(first.Cession.ID); e != nil {
		t.Fatal(e)
	}
	if e = b.CollectTransferred(first.Cession.ID); e != nil {
		t.Fatal(e)
	}
	if e = b.InstallCausal(old); e != nil {
		t.Fatal(e)
	}
	z, _ := recordZone(record)
	snap, e := a.Snapshot(z)
	if e != nil || len(snap.Records) != 1 {
		t.Fatal("old collector deleted returned ACK", e)
	}
	old.Ticket = 0
	fresh, e := b.ReserveCausal(old)
	if e != nil {
		t.Fatal(e)
	}
	if fresh.Ticket <= first.Cession.Ticket {
		t.Fatal("ticket reused")
	}
	packet := first
	packet.Snapshots = nil
	if _, e = b.PreloadCausal(packet); e == nil {
		t.Fatal("old preload confirmed new slot")
	}
	if _, e = b.Write(fmt.Sprint("replayed", fresh.Ticket), record); e == nil {
		t.Fatal("old replay reactivated writer")
	}
}

func routingStore(t *testing.T) (*Store, causal.Genesis) {
	t.Helper()
	s, err := OpenWithOptions(filepath.Join(t.TempDir(), "routing.db"), "D", "test", 1<<26, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h, err := s.Endpoint("D")
	if err != nil {
		t.Fatal(err)
	}
	g := causal.Genesis{Cluster: "test", Holder: h}
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, g); err != nil {
		t.Fatal(err)
	}
	return s, g
}

func TestLocalTicketsPhysicalPointBeforePlacementPublication(t *testing.T) {
	source, _, g := ticketStores(t)
	receiver := openTestData(t, filepath.Join(t.TempDir(), "physical-A"), "A")
	defer receiver.Close()
	if e := receiver.InitCausalReceiver(tree.VNodeDescriptor{Name: "A", ID: tree.ID{1}}, g); e != nil {
		t.Fatal(e)
	}
	endpoint, e := receiver.Endpoint("future-A")
	if e != nil {
		t.Fatal(e)
	}
	endpoint.Epoch = 1
	if e = source.BeginCausalMove(endpoint); e != nil {
		t.Fatal(e)
	}
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "original"}
	if _, e = source.Write("original", record); e != nil {
		t.Fatal(e)
	}
	op, e := source.BeginCausal(context.Background(), g.Right().ID, endpoint, tree.Region{})
	if e != nil {
		t.Fatal(e)
	}
	c, e := receiver.ReserveCausal(op.Cession)
	if e != nil {
		t.Fatal(e)
	}
	if e = source.BindCausal(c.ID, c.Ticket); e != nil {
		t.Fatal(e)
	}
	op.Cession = c
	if _, e = receiver.PreloadCausal(op); e != nil {
		t.Fatal(e)
	}
	if e = source.ConfirmCausal(c.ID); e != nil {
		t.Fatal(e)
	}
	z, _ := recordZone(record)
	snaps, e := source.FreezeZones(c.ID, []tree.ZoneKey{z})
	if e != nil {
		t.Fatal(e)
	}
	if e = receiver.StageZones(c.ID, source.node, snaps); e != nil {
		t.Fatal(e)
	}
	proofs, e := source.EmitZoneTransfers(c.ID, []tree.ZoneKey{z})
	if e != nil {
		t.Fatal(e)
	}
	if e = receiver.InstallZones(c.ID, proofs); e != nil {
		t.Fatal(e)
	}
	current, e := receiver.Endpoint("future-A")
	if e != nil || current.Epoch != 1 {
		t.Fatal("point ACK must advertise future epoch", current, e)
	}
	record.ID = "early-physical"
	if _, e = receiver.Write("early-physical", record); e != nil {
		t.Fatal("physical point must write before MOVE publication", e)
	}
	routing, e := receiver.CausalRouting()
	if e != nil {
		t.Fatal(e)
	}
	published, e := routing.Placement("A")
	if e != nil || published.Storage != g.Holder.Storage {
		t.Fatal("placement published early", published, e)
	}
}

func TestLocalTicketsAdmissionBudgetPreservesConfirmedSlots(t *testing.T) {
	source, target, g := ticketStores(t)
	confirmed := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	base := confirmed.Cession
	base.Ticket = 0
	for i := 1; i <= MaxReceiverAdmissions+20; i++ {
		c := base
		c.ID = causal.TransferID{Actor: base.Source.Storage, Sequence: uint64(1000 + i)}
		if _, e := target.ReserveCausal(c); e != nil {
			t.Fatal(e)
		}
	}
	state, e := target.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	if len(state.Transfers) != MaxReceiverAdmissions {
		t.Fatal("unbounded admissions", len(state.Transfers))
	}
	saved, ok := state.Transfers[confirmed.Cession.ID]
	if !ok || saved.Phase != causal.TransferReady {
		t.Fatal("confirmed slot evicted")
	}
	if _, e = source.ForceFreezeRegion(confirmed.Cession.ID); e != nil {
		t.Fatal("confirmed agreement lost under replay pressure", e)
	}
}

func TestLocalTicketsWritesUseCachedLocalPermissions(t *testing.T) {
	store, g := routingStore(t)
	var members []causal.Member
	for i := 1; i <= 64; i++ {
		members = append(members, causal.Member{VNode: tree.VNodeDescriptor{Name: fmt.Sprintf("peer-%02d", i), ID: tree.ID{byte(i + 1)}}, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: fmt.Sprintf("peer-%02d", i), Storage: fmt.Sprintf("disk-%02d", i)}, Host: fmt.Sprintf("host-%02d", i)}})
	}
	if e := store.MergeMembership(causal.MembershipDelta{Members: members}); e != nil {
		t.Fatal(e)
	}
	if _, e := store.CausalRouting(); e != nil {
		t.Fatal(e)
	}
	before := controlValueReads.Load()
	store.causalCacheMu.Lock()
	defer store.causalCacheMu.Unlock()
	for i := range 100 {
		record := Record{Collection: "Demo000000000000", Location: "aa", ID: fmt.Sprint(i)}
		if _, e := store.Write(fmt.Sprint("cached", i), record); e != nil {
			t.Fatal("routing placement must not revoke installed native authority", e)
		}
	}
	if after := controlValueReads.Load(); after != before {
		t.Fatalf("100 writes reread %d control documents", after-before)
	}
	state, e := store.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	if state.Permissions.Native[0] != g.Right() {
		t.Fatal("membership installed/revoked authority")
	}
}

func TestLocalTicketsCancelledCleanupSurvivesLostReplyAndRestart(t *testing.T) {
	source, target, g := ticketStores(t)
	op := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	if e := source.AbortCausal(op.Cession.ID); e != nil {
		t.Fatal(e)
	}
	state, e := source.CausalState()
	if e != nil || state.Transfers[op.Cession.ID].Phase != causal.TransferCancelled {
		t.Fatal("source forgot cancellation cleanup", e)
	}
	if _, e = source.ForceFreezeRegion(op.Cession.ID); !errors.Is(e, ErrNotReady) {
		t.Fatal("cancelled source fenced", e)
	}
	// Receiver cancellation commits but its reply is lost. Source restart must
	// retain the exact ticket and repeat cleanup, never reopen old authority.
	if e = target.CancelCausal(op.Cession); e != nil {
		t.Fatal(e)
	}
	path := source.path
	if e = source.Close(); e != nil {
		t.Fatal(e)
	}
	source = openTestData(t, path, "A")
	defer source.Close()
	state, e = source.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	saved := state.Transfers[op.Cession.ID]
	if saved.Phase != causal.TransferCancelled || saved.Cession.Ticket != op.Cession.Ticket {
		t.Fatal("cancel intent lost across restart")
	}
	if e = target.CancelCausal(saved.Cession); e != nil {
		t.Fatal("lost cancellation reply not idempotent", e)
	}
	if e = source.FinishAbortCausal(op.Cession.ID); e != nil {
		t.Fatal(e)
	}
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "still-source"}
	if _, e = source.Write("still-source", record); e != nil {
		t.Fatal("cancellation consumed installed input", e)
	}
}

func TestLocalTicketsPointReferralsDoNotBlockUnmovedScopes(t *testing.T) {
	source, target, g := ticketStores(t)
	left, _ := tree.NewZoneKey("Demo000000000000", "aa")
	right, _ := tree.NewZoneKey("Demo000000000000", "zz")
	if left.ID == right.ID {
		t.Fatal("fixture needs disjoint geometric points")
	}
	for _, z := range []tree.ZoneKey{left, right} {
		if _, e := source.Write(z.Location, Record{Collection: z.Collection, Location: z.Location, ID: z.Location}); e != nil {
			t.Fatal(e)
		}
	}
	op := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	snaps, e := source.FreezeZones(op.Cession.ID, []tree.ZoneKey{left})
	if e != nil {
		t.Fatal(e)
	}
	if e = target.StageZones(op.Cession.ID, source.node, snaps); e != nil {
		t.Fatal(e)
	}
	proofs, e := source.EmitZoneTransfers(op.Cession.ID, []tree.ZoneKey{left})
	if e != nil {
		t.Fatal(e)
	}
	if e = target.InstallZones(op.Cession.ID, proofs); e != nil {
		t.Fatal(e)
	}
	e = source.view(func(tx *Tx) error {
		c, e := source.causalSnapshot(tx)
		if e != nil {
			return e
		}
		unmoved, e := inventoryReferrals(tx, c, tree.Point(right.ID), nil)
		if e != nil {
			return e
		}
		if len(unmoved) != 0 {
			t.Fatal("unmoved scope requires unrelated receiver")
		}
		moved, e := inventoryReferrals(tx, c, tree.Point(left.ID), nil)
		if e != nil {
			return e
		}
		if len(moved) != 1 || moved[0].Region != tree.Point(left.ID) {
			t.Fatal("moved point missing scoped mandatory receiver", moved)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}

// Regional consolidation has no second data path. Old bytes must not be
// accepted here after point ACKs have already made the receiver writable.
func TestRegionalStageRejectsSecondSnapshotCopy(t *testing.T) {
	source, target, g := ticketStores(t)
	record := Record{Collection: "Demo000000000000", Location: "A", ID: "old"}
	if _, e := source.Write("old", record); e != nil {
		t.Fatal(e)
	}
	op := ticketAdmission(t, source, target, g.Right().ID, tree.Region{})
	c := ticketFinish(t, source, target, op.Cession.ID)
	packet, _, e := source.LoadCausalTransfer(c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(packet.Snapshots) == 0 {
		t.Fatal("fixture has no obsolete snapshot")
	}
	packet.Cession = c
	before := target.db.Seq()
	if e := target.StageCausal(packet); !errors.Is(e, ErrConflict) {
		t.Fatalf("regional payload accepted: %v", e)
	}
	if target.db.Seq() != before {
		t.Fatal("rejected second copy changed durable data")
	}
}
