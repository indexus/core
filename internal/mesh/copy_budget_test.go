package mesh

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// hostBudgets opens the same budget directory once per simulated process:
// flock is per open file, so two handles exclude each other as two processes.
func hostBudgets(t testing.TB, slots, processes int) []*CopyBudget {
	t.Helper()
	dir := t.TempDir()
	out := make([]*CopyBudget, processes)
	for i := range out {
		b, err := NewCopyBudget(dir, slots)
		if err != nil || b == nil {
			t.Fatalf("budget: %v %v", b, err)
		}
		out[i] = b
	}
	return out
}

func shrinkCopyWait(t testing.TB, wait time.Duration) {
	t.Helper()
	oldWait, oldPoll := copySlotWait, copySlotPoll
	copySlotWait, copySlotPoll = wait, time.Millisecond
	t.Cleanup(func() { copySlotWait, copySlotPoll = oldWait, oldPoll })
}

func TestCopyBudgetIsOffWithoutADirectory(t *testing.T) {
	b, err := NewCopyBudget("", 1)
	if b != nil || err != nil {
		t.Fatalf("budget %v, %v", b, err)
	}
	for i := 0; i < 3; i++ { // never released: an off budget has no slot to run out of
		_ = b.acquire(context.Background(), "op", time.Hour)
	}
	if DefaultCopySlots() < 1 {
		t.Fatal("no default slot")
	}
}

// Six copies from two processes of one host, two slots: never more than two
// at once, and all of them run.
func TestCopyBudgetBoundsCopiesAcrossProcesses(t *testing.T) {
	budgets := hostBudgets(t, 2, 2)
	var inside, most, done atomic.Int32
	entered := make(chan struct{}, 6)
	releaseWave := make(chan struct{})
	var copies sync.WaitGroup
	for i := 0; i < 6; i++ {
		copies.Add(1)
		go func(i int) {
			defer copies.Done()
			release := budgets[i%2].acquire(context.Background(), string(rune('a'+i)), time.Minute)
			now := inside.Add(1)
			for old := most.Load(); now > old && !most.CompareAndSwap(old, now); old = most.Load() {
			}
			entered <- struct{}{}
			<-releaseWave
			inside.Add(-1)
			done.Add(1)
			release()
		}(i)
	}
	// Hold the first admissions until both lanes are occupied. A sleep
	// measures scheduler timing and can serialize healthy lanes under load.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-timer.C:
			close(releaseWave)
			copies.Wait()
			t.Fatal("two copy lanes did not become available together")
		}
	}
	close(releaseWave)
	copies.Wait()
	if most.Load() != 2 || done.Load() != 6 {
		t.Fatalf("at most %d copies at once, %d done; want 2 and 6", most.Load(), done.Load())
	}
}

// A process that dies gives its slot back: the kernel closes its files. The
// name it left on the slot is not taken for a live holder.
func TestCopyBudgetCrashedHolderReleasesItsSlot(t *testing.T) {
	budgets := hostBudgets(t, 1, 2)
	held, shared := budgets[0].take("op-1")
	if held == nil || shared {
		t.Fatalf("first take: %v %v", held, shared)
	}
	if other, _ := budgets[1].take("op-2"); other != nil {
		t.Fatal("two holders of one slot")
	}
	held.Close() // the crash: no release, the name stays on the slot
	again, shared := budgets[1].take("op-1")
	if again == nil || shared {
		t.Fatalf("slot of a dead holder: held=%v shared=%v", again, shared)
	}
	again.Close()
}

// The wait for a slot is bounded: the copy then proceeds without one and is
// counted. A caller that may not wait proceeds at once.
func TestCopyBudgetWaitIsBoundedAndCounted(t *testing.T) {
	budgets := hostBudgets(t, 1, 2)
	shrinkCopyWait(t, 60*time.Millisecond)
	release := budgets[0].acquire(context.Background(), "holder", copySlotWait)
	waits, timeouts, busy := metrics.Handoffs.CopySlotWaits.Load(), metrics.Handoffs.CopySlotTimeouts.Load(), metrics.Handoffs.CopySlotBusy.Load()

	started := time.Now()
	budgets[1].acquire(context.Background(), "late", copySlotWait)()
	if waited := time.Since(started); waited < copySlotWait || waited > 5*time.Second {
		t.Fatalf("waited %v, want about %v", waited, copySlotWait)
	}
	if metrics.Handoffs.CopySlotWaits.Load() != waits+1 || metrics.Handoffs.CopySlotTimeouts.Load() != timeouts+1 {
		t.Fatal("a copy without a slot was not counted")
	}
	started = time.Now()
	budgets[1].acquire(context.Background(), "no-wait", 0)()
	if time.Since(started) > 40*time.Millisecond || metrics.Handoffs.CopySlotBusy.Load() != busy+1 {
		t.Fatalf("a caller that may not wait waited %v", time.Since(started))
	}
	// A cancelled wait ends at once; the slot is free again after release.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	copySlotWait = time.Minute
	started = time.Now()
	budgets[1].acquire(ctx, "cancelled", copySlotWait)()
	if time.Since(started) > 5*time.Second {
		t.Fatal("a cancelled wait went on")
	}
	release()
	if held, _ := budgets[1].take("next"); held == nil {
		t.Fatal("released slot not free")
	} else {
		held.Close()
	}
}

// A source and a receiver of one transfer on the same host use one slot: the
// receiver shares it, and leaves it to the source when it is done.
func TestCopyBudgetCoResidentTransferNeedsOneSlot(t *testing.T) {
	budgets := hostBudgets(t, 1, 2)
	source, receiver := budgets[0], budgets[1]
	shared := metrics.Handoffs.CopySlotShared.Load()
	release := source.acquire(context.Background(), "op-1", time.Minute)

	started := time.Now()
	receiver.acquire(context.Background(), "op-1", time.Minute)() // would wait a minute for a second slot
	if time.Since(started) > 5*time.Second || metrics.Handoffs.CopySlotShared.Load() != shared+1 {
		t.Fatalf("the receiver of the same transfer did not share its source's slot (%v)", time.Since(started))
	}
	if held, _ := receiver.take("op-2"); held != nil {
		t.Fatal("the receiver's release freed its source's slot")
	}
	release()
	if held, _ := receiver.take("op-2"); held == nil {
		t.Fatal("slot not free after the source released it")
	} else {
		held.Close()
	}
}

// Two donors and a receiver on one host, one slot. The first donor gives to
// the co-resident receiver, the second to another host. The second donor's
// grain does not start while the first one copies; the receiver shares the
// slot of the grain it receives; both transfers complete.
func TestCoResidentDonorsShareTheHostCopyBudget(t *testing.T) {
	net, nodes := meshFixture(t, 4)
	first, second, receiver, remote := nodes[0], nodes[1], nodes[2], nodes[3]
	budgets := hostBudgets(t, 1, 3)
	for i, node := range nodes[:3] {
		node.SetCopyBudget(budgets[i])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Give the second donor a right of its own, before the budget matters.
	genesis, _ := first.Store.CausalState()
	if err := first.handoff(ctx, Request{Kind: "handoff", Input: genesis.Genesis.Right().ID, Target: second.Self, Region: tree.Region{Mask: tree.ID{192}, Value: tree.ID{64}}}); err != nil {
		t.Fatal(err)
	}
	rightAt := func(s *Service, first byte) causal.Digest {
		state, _ := s.Store.CausalState()
		for _, right := range state.Permissions.Native {
			if right.Region.Contains(tree.ID{first, 1}) {
				return right.ID
			}
		}
		t.Fatalf("no right at %d", first)
		return causal.Digest{}
	}
	for donor, at := range map[*Service]byte{first: 160, second: 100} {
		address := tree.ID{at, 1}
		if _, err := donor.Store.Write("r", storage.Record{Collection: "Demo000000000000", Location: tree.BASE64.Encode(address[:]), ID: "r"}); err != nil {
			t.Fatal(err)
		}
	}

	// The first donor's copy is held open until the second donor reaches its
	// receiver, or for long enough to show that it does not.
	copying, secondAdmits := make(chan struct{}), make(chan struct{})
	var overlap atomic.Bool
	var once, admitOnce sync.Once
	first.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if r.Kind == "preload" && len(r.Transfer.Snapshots) > 0 {
			once.Do(func() {
				close(copying)
				select {
				case <-secondAdmits:
					overlap.Store(true)
				case <-time.After(300 * time.Millisecond):
				}
			})
		}
		return net.Exchange(ctx, host, r)
	})
	second.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if r.Kind == "admit" {
			admitOnce.Do(func() { close(secondAdmits) })
		}
		return net.Exchange(ctx, host, r)
	})
	shared, timeouts := metrics.Handoffs.CopySlotShared.Load(), metrics.Handoffs.CopySlotTimeouts.Load()
	errs := make(chan error, 2)
	go func() {
		errs <- first.handoff(ctx, Request{Kind: "handoff", Input: rightAt(first, 160), Target: receiver.Self, Region: tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}})
	}()
	select {
	case <-copying:
	case <-ctx.Done():
		t.Fatal("the first donor never copied")
	}
	go func() {
		errs <- second.handoff(ctx, Request{Kind: "handoff", Input: rightAt(second, 100), Target: remote.Self, Region: tree.Region{Mask: tree.ID{192}, Value: tree.ID{64}}})
	}()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("handoff: %v", err)
		}
	}
	if overlap.Load() {
		t.Fatal("the second donor started its grain while the first one copied")
	}
	if metrics.Handoffs.CopySlotShared.Load() == shared {
		t.Fatal("the co-resident receiver did not share its source's slot")
	}
	if metrics.Handoffs.CopySlotTimeouts.Load() != timeouts {
		t.Fatal("a donor copied without a slot")
	}
	rightAt(receiver, 160)
	rightAt(remote, 100)
}

func TestWarmCopyCannotBypassExhaustedHostBudget(t *testing.T) {
	budgets := hostBudgets(t, 1, 2)
	held, _ := budgets[0].take("holder")
	defer held.Close()
	release, err := budgets[1].reserve(context.Background(), "warm", time.Millisecond, true)
	if !errors.Is(err, storage.ErrNotReady) {
		t.Fatal("unfenced copy bypassed admission", err)
	}
	release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := budgets[1].reserve(ctx, "canceled", time.Second, true); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled copy was admitted", err)
	}
}

func TestCopyPressurePacesOnlyBetweenUnfrozenLots(t *testing.T) {
	budgets := hostBudgets(t, 1, 2)
	budgets[0].pressure.Store(&copyPressure{used: 80, target: 60})
	if got := budgets[0].cooldown(time.Second); got < 3999*time.Millisecond || got > 4001*time.Millisecond {
		t.Fatal("wrong pressure duty cycle", got)
	}
	held, _ := budgets[0].take("source")
	defer held.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := budgets[0].pace(ctx, time.Second, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("paced wait did not cancel", err)
	}
	if other, _ := budgets[1].take("other-source"); other != nil {
		other.Close()
		t.Fatal("another process bypassed the paced source's lane")
	}
	if err := budgets[0].pace(context.Background(), time.Second, true); err != nil {
		t.Fatal(err)
	}
	budgets[0].pressure.Store(&copyPressure{used: 30, target: 60})
	if budgets[0].cooldown(time.Second) != 0 {
		t.Fatal("idle host was paced")
	}
}

func TestUnadmittedHandoffKeepsRightUnpinnedAndWritable(t *testing.T) {
	_, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	budgets := hostBudgets(t, 1, 2)
	source.SetCopyBudget(budgets[1])
	held, _ := budgets[0].take("other-copy")
	shrinkCopyWait(t, 5*time.Millisecond)
	state, _ := source.Store.CausalState()
	request := Request{Kind: "handoff", Input: state.Genesis.Right().ID, Region: tree.Region{}, Target: target.Self}
	for _, subdivide := range []bool{true, false} {
		request.Subdivide = subdivide
		err := source.handoff(context.Background(), request)
		if !errors.Is(err, storage.ErrNotReady) {
			held.Close()
			t.Fatal("budget exhaustion did not defer warm handoff", err)
		}
		after, err := source.Store.CausalState()
		if err != nil || len(after.Transfers) != 0 {
			held.Close()
			t.Fatalf("unadmitted proposal pinned a durable transfer: %v, %v", after.Transfers, err)
		}
		if _, ok := after.Permissions.Find(request.Input); !ok {
			held.Close()
			t.Fatal("unadmitted proposal partitioned its input")
		}
	}
	if frozen, err := source.Store.FrozenOutgoing(); err != nil || len(frozen) != 0 {
		held.Close()
		t.Fatal("admission wait fenced writes", frozen, err)
	}
	row := storage.Record{Collection: "Demo000000000000", Location: "a", ID: "during-admission"}
	if _, _, applied, err := source.Store.ApplyRecord("during-admission", row); err != nil || !applied {
		held.Close()
		t.Fatal("waiting source stopped writing", err)
	}
	held.Close()
	if err := source.handoff(context.Background(), request); err != nil && !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if _, _, applied, err := target.Store.ApplyRecord("after-install", storage.Record{Collection: row.Collection, Location: row.Location, ID: "after-install"}); err != nil || !applied {
		t.Fatal("receiver cannot write", err)
	}
	if _, _, _, err := source.Store.ApplyRecord("old-source", row); !errors.Is(err, storage.ErrNotOwner) {
		t.Fatal("source kept write authority", err)
	}
}

// Reproduce recovery on a donor with one warm operation, one sealed operation
// and a busy host lane. The sealed receiver must be contacted before recovery
// spends any admission interval on the other operation.
func TestRecoveryFinishesSealedBeforeWaitingForWarmCopy(t *testing.T) {
	net, nodes := meshFixture(t, 4)
	source, seed, warm, sealed := nodes[0], nodes[1], nodes[2], nodes[3]
	ctx := context.Background()
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	quarter := tree.Region{Mask: tree.ID{192}, Value: tree.ID{64}}
	if err := source.handoff(ctx, Request{Input: state.Genesis.Right().ID, Target: seed.Self, Region: quarter}); err != nil {
		t.Fatal(err)
	}
	state, err = source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	a, okA := state.Permissions.At(tree.ID{160, 1})
	b, okB := state.Permissions.At(tree.ID{24, 1})
	if !okA || !okB || a.ID == b.ID {
		t.Fatal("need two independent rights")
	}
	confirmedTransfer(t, source, warm, a.ID, a.Region)
	op := confirmedTransfer(t, source, sealed, b.ID, b.Region)
	if _, err := source.Store.ForceFreezeRegion(op.Cession.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Store.SealCausalRegion(op.Cession.ID); err != nil {
		t.Fatal(err)
	}
	budgets := hostBudgets(t, 1, 2)
	source.SetCopyBudget(budgets[0])
	held, _ := budgets[1].take("unrelated")
	defer held.Close()
	shrinkCopyWait(t, 5*time.Millisecond)
	waits := metrics.Handoffs.CopySlotWaits.Load()
	reached := false
	source.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if host == sealed.Self.Host && r.Kind == "install" {
			reached = true
			if metrics.Handoffs.CopySlotWaits.Load() != waits {
				t.Error("warm recovery waited before sealed install")
			}
		}
		return net.Exchange(ctx, host, r)
	})
	if err := source.Resume(ctx); !errors.Is(err, storage.ErrNotReady) {
		t.Fatalf("expected deferred warm transfer, got %v", err)
	}
	state, err = source.Store.CausalState()
	if err != nil || !reached || state.Transfers[op.Cession.ID].Phase != causal.TransferInstalled {
		t.Fatalf("sealed transfer did not finish: %v", err)
	}
}
