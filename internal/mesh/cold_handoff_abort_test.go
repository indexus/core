package mesh

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestHandoffToColdDrainingOrRetiredTargetAbortsCleanly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, nodes := meshFixture(t, 2)
	nodes[1].VNode.ID = tree.ID{0, 1}
	for i, location := range []string{"0", "00", "000", tree.EncodeName(tree.ID{192})} {
		r := storage.Record{Collection: "0000000000000001", Location: location, ID: fmt.Sprintf("row-%d", i), Metrics: []float64{float64(i)}}
		if _, err := nodes[0].Write(ctx, "seed-"+r.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].Sync(ctx, []string{nodes[1].Self.Host}); err != nil {
		t.Fatal(err)
	}

	// Start a cold drain on nodes[1]: its receiver is now closed.
	if _, err := nodes[1].Store.BeginColdDrain("cold-op-test", nodes[0].Self, storage.ColdLimits{}); err != nil {
		t.Fatal(err)
	}

	// Join and Rebalance on a draining vnode must stop instead of asking donors
	// to hand off regions to a closed receiver.
	if err := nodes[1].Join(ctx); !errors.Is(err, storage.ErrReceiverClosed) {
		t.Fatalf("draining Join: got %v, want ErrReceiverClosed", err)
	}
	if err := nodes[1].Rebalance(ctx); err != nil {
		t.Fatalf("draining Rebalance: got %v, want nil", err)
	}

	// Simulate an in-flight handoff RPC arriving at donor nodes[0] for draining
	// target nodes[1]: donor begins the transfer, gets ErrReceiverClosed from
	// nodes[1], aborts cleanly, and leaves zero open/frozen transfers.
	state, err := nodes[0].Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Permissions.Native) == 0 {
		t.Fatal("expected native donor right")
	}
	right := state.Permissions.Native[0]
	err = nodes[0].handoff(ctx, Request{Kind: "handoff", Input: right.ID, Region: right.Region, Target: nodes[1].Self, TargetVNode: nodes[1].VNode})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("handoff to draining target: %v", err)
	}
	frozen, err := nodes[0].Store.FrozenOutgoing()
	if err != nil || len(frozen) != 0 {
		t.Fatalf("donor left frozen transfers after abort: %v %v", frozen, err)
	}
	after, err := nodes[0].Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range after.Transfers {
		if tr.Phase != causal.TransferInstalled && tr.Cession.Target.Owner == nodes[1].Self.Owner {
			t.Fatal("aborted target still retains an open source operation")
		}
	}

	if err := nodes[0].Resume(ctx); err != nil {
		t.Fatalf("donor Resume after abort: %v", err)
	}
	// Donor remains writable on its owned keyspace.
	if _, err := nodes[0].Write(ctx, "post-abort-write", storage.Record{
		Collection: "0000000000000001", Location: "00", ID: "after-abort", Metrics: []float64{42},
	}); err != nil {
		t.Fatalf("donor write after abort failed: %v", err)
	}
}

func TestResumeAbortsUnemittedTransferToRetiredTarget(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, nodes := meshFixture(t, 2)
	nodes[1].VNode.ID = tree.ID{0, 1}
	for i, location := range []string{"0", "00", tree.EncodeName(tree.ID{192})} {
		r := storage.Record{Collection: "0000000000000001", Location: location, ID: fmt.Sprintf("r-%d", i), Metrics: []float64{float64(i)}}
		if _, err := nodes[0].Write(ctx, "seed-"+r.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].Sync(ctx, []string{nodes[1].Self.Host}); err != nil {
		t.Fatal(err)
	}

	// Complete a real cold fold of nodes[1] into nodes[0], retiring nodes[1].
	if err := nodes[1].Compact(ctx, ColdConfig{}); err != nil {
		t.Fatal(err)
	}

	// A late handoff RPC targeting the retired vnode must be rejected upfront.
	state, err := nodes[0].Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Permissions.Native) == 0 {
		t.Fatal("expected native donor right")
	}
	right := state.Permissions.Native[0]
	_, err = nodes[0].Handle(ctx, Request{Kind: "handoff", Input: right.ID, Region: right.Region, Target: nodes[1].Self, TargetVNode: nodes[1].VNode})
	if !errors.Is(err, causal.ErrConflict) {
		t.Fatalf("late handoff to retired vnode: %v", err)
	}
	stale, err := nodes[0].Store.BeginCausal(ctx, right.ID, nodes[1].Self, right.Region)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].Resume(ctx); err != nil {
		t.Fatalf("Resume with stale transfer to retired target: %v", err)
	}
	after, err := nodes[0].Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := after.Transfers[stale.Cession.ID]; still {
		t.Fatalf("Resume did not abort stale transfer %q to retired target", stale.Cession.ID)
	}
}

func TestAbortCausalRefusesAfterEmit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, nodes := meshFixture(t, 2)
	nodes[1].VNode.ID = tree.ID{0, 1}
	if _, err := nodes[0].Write(ctx, "w1", storage.Record{
		Collection: "0000000000000001", Location: "00", ID: "id1", Metrics: []float64{1},
	}); err != nil {
		t.Fatal(err)
	}
	state, err := nodes[0].Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	right := state.Permissions.Native[0]
	offer := confirmedTransfer(t, nodes[0], nodes[1], right.ID, right.Region)
	op := offer.Cession.ID
	zones, err := nodes[0].Store.TransferZones(op)
	if err != nil || len(zones) == 0 {
		t.Fatalf("zones %v %d", err, len(zones))
	}
	if _, err = nodes[0].Store.FreezeZones(op, []tree.ZoneKey{zones[0].Zone}); err != nil {
		t.Fatal(err)
	}
	if err = nodes[0].Store.AbortCausal(op); !errors.Is(err, storage.ErrNotReady) {
		t.Fatalf("abort after first fence: %v", err)
	}
	if _, err = nodes[0].Store.EmitZoneTransfers(op, []tree.ZoneKey{zones[0].Zone}); err != nil {
		t.Fatal(err)
	}
	if err = nodes[0].Store.AbortCausal(op); !errors.Is(err, storage.ErrNotReady) {
		t.Fatalf("abort after point emit: %v", err)
	}
}

func TestCancelledAdmissionRecoversLostCancelACK(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	offer := confirmedTransfer(t, source, target, state.Genesis.Right().ID, tree.Region{})
	if err = source.Store.AbortCausal(offer.Cession.ID); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("cancel ACK lost after durable receiver cleanup")
	var once sync.Once
	net.setAfter(func(r Request) error {
		if r.Kind != "cancel" {
			return nil
		}
		var err error
		once.Do(func() { err = lost })
		return err
	})
	if err = source.Resume(context.Background()); !errors.Is(err, lost) {
		t.Fatalf("lost cancel not exposed: %v", err)
	}
	pending, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Transfers[offer.Cession.ID].Phase != causal.TransferCancelled {
		t.Fatal("source forgot cancellation before ACK")
	}
	receiver, err := target.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := receiver.Transfers[offer.Cession.ID]; exists {
		t.Fatal("receiver confirmed slot was not cleaned")
	}
	if err = source.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err = source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := pending.Transfers[offer.Cession.ID]; exists {
		t.Fatal("source did not collect acknowledged cancellation")
	}
	if _, err = target.Store.PreloadCausal(offer); !errors.Is(err, storage.ErrNotReady) {
		t.Fatalf("closed admission must not confirm a preload: %v", err)
	}
	row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "after-cancel"}
	if _, err = source.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	if _, err = target.Store.Write("must-not-apply", row); err == nil {
		t.Fatal("replayed canceled preload acquired write rights")
	}
}

// Closing then reopening intake may discard header-only admissions. The donor
// must replan a fresh operation, not retry a permanently closed ticket forever.
func TestPrunedTentativeAdmissionReplansBeforeAnyFence(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := source.Sync(ctx, []string{target.Self.Host}); err != nil {
		t.Fatal(err)
	}
	rights, err := source.Store.Rights(tree.Region{})
	if err != nil || len(rights) == 0 {
		t.Fatalf("source rights %v %v", rights, err)
	}
	input := rights[0]
	var first sync.Once
	var old causal.TransferID
	net.setBefore(func(r Request) error {
		if r.Kind != "preload" {
			return nil
		}
		var fault error
		first.Do(func() {
			old = r.Transfer.Cession.ID
			if _, fault = target.Store.BeginColdDrain("prune-unconfirmed", source.Self, storage.ColdLimits{}); fault != nil {
				return
			}
			fault = target.Store.AbortColdDrain()
		})
		return fault
	})
	request := Request{Kind: "handoff", Input: input.ID, Region: input.Region, Target: target.Self, TargetVNode: target.VNode}
	if err = source.handoff(ctx, request); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("pruned admission did not replan: %v", err)
	}
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := state.Transfers[old]; exists {
		t.Fatal("source retained unusable old ticket")
	}
	if _, exists := state.Permissions.Find(input.ID); !exists {
		t.Fatal("pre-fence pruning consumed source permission")
	}
	if frozen, e := source.Store.FrozenOutgoing(); e != nil || len(frozen) > 0 {
		t.Fatalf("pruned admission fenced data: %v %v", frozen, e)
	}
	if err = source.handoff(ctx, request); err != nil {
		t.Fatalf("new unique operation did not complete: %v", err)
	}
	targetState, err := target.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	for id, tr := range targetState.Transfers {
		if tr.Cession.Input.ID == input.ID && id == old {
			t.Fatal("new handoff rebound the closed source operation")
		}
	}
}

func TestWarmCapacityRefusalBeforeFenceReleasesSourceInput(t *testing.T) {
	t.Parallel()
	net, source, target := streamFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	input := state.Permissions.Native[0]
	var refused atomic.Bool
	net.setBefore(func(r Request) error {
		if r.Kind == "preload" && len(r.Transfer.Snapshots) > 0 {
			refused.Store(true)
			return storage.ErrDataCapacity
		}
		return nil
	})
	request := Request{Kind: "handoff", Input: input.ID, Region: input.Region, Target: target.Self, TargetVNode: target.VNode}
	if err = source.handoff(ctx, request); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("warm capacity refusal did not cancel safely: %v", err)
	}
	if !refused.Load() {
		t.Fatal("warm copy refusal not injected")
	}
	after, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := after.Permissions.Find(input.ID); !exists {
		t.Fatal("warm refusal consumed source right")
	}
	for _, tr := range after.Transfers {
		if tr.Phase != causal.TransferInstalled {
			t.Fatal("warm refusal left a pinned source operation")
		}
	}
	if frozen, e := source.Store.FrozenOutgoing(); e != nil || len(frozen) > 0 {
		t.Fatalf("warm refusal left a fence: %v %v", frozen, e)
	}
	row := storage.Record{Collection: "collection-00000", Location: "aa", ID: "ACK-after-warm-refusal"}
	if _, err = source.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	if _, err = target.Store.Write("must-not-apply", row); err == nil {
		t.Fatal("warm admission installed authority")
	}
	net.setBefore(nil)
	if err = source.handoff(ctx, request); err != nil {
		t.Fatalf("fresh handoff after capacity returned failed: %v", err)
	}
	rows, err := target.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, snap := range rows {
		count += len(snap.Records)
	}
	if count != 2 {
		t.Fatalf("warm refusal lost ACKs: %d", count)
	}
}
