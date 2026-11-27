package mesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestIndependentTokenFinishesWhileAnotherReceiverIsPartitioned(t *testing.T) {
	net, nodes := meshFixture(t, 4)
	source, seed, a, b := nodes[0], nodes[1], nodes[2], nodes[3]
	b.VNode.ID = tree.ID{17, 1}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blocked := make(chan struct{}, 1)
	releaseA := make(chan struct{})
	source.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if host == a.Self.Host && r.Kind == "install-zone" {
			select {
			case blocked <- struct{}{}:
			default:
			}
			select {
			case <-ctx.Done():
				return Response{}, ctx.Err()
			case <-releaseA:
				return Response{}, errors.New("test receiver still unavailable")
			}
		}
		return net.Exchange(ctx, host, r)
	})
	state, _ := source.Store.CausalState()
	// One completed transfer leaves two disjoint, installed source tokens.
	if err := source.handoff(ctx, Request{Kind: "handoff", Input: state.Genesis.Right().ID, Target: seed.Self, Region: tree.Region{Mask: tree.ID{192}, Value: tree.ID{64}}}); err != nil {
		t.Fatal(err)
	}
	state, _ = source.Store.CausalState()
	var tokenA, tokenB causal.Digest
	for _, right := range state.Permissions.Native {
		if right.Region.Contains(tree.ID{160, 1}) {
			tokenA = right.ID
		}
		if right.Region.Contains(tree.ID{24, 1}) {
			tokenB = right.ID
		}
	}
	if tokenA == tokenB || tokenA == (causal.Digest{}) || tokenB == (causal.Digest{}) {
		t.Fatal("fixture lacks independent rights")
	}
	record := func(first byte, id string) storage.Record {
		address := tree.ID{first, 1}
		return storage.Record{Collection: "Demo000000000000", Location: tree.BASE64.Encode(address[:]), ID: id}
	}
	for _, r := range []storage.Record{record(160, "a"), record(24, "b")} {
		if _, err := source.Store.Write(r.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	requestA := Request{Kind: "handoff", Input: tokenA, Target: a.Self, Region: tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}}
	// The caller stops waiting; the source worker remains blocked on A.
	_ = source.handoffAsync(ctx, requestA, time.Millisecond)
	source.jobs.mu.Lock()
	pendingA := source.jobs.jobs[handoffKey(requestA)]
	source.jobs.mu.Unlock()
	defer func() {
		close(releaseA)
		if pendingA != nil {
			select {
			case <-pendingA.done:
			case <-time.After(time.Second):
				t.Error("A worker did not stop")
			}
		}
	}()
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("A did not reach its frozen stage")
	}
	requestB := Request{Kind: "handoff", Input: tokenB, Target: b.Self, TargetVNode: b.VNode, Region: tree.Region{Mask: tree.ID{208}, Value: tree.ID{16}}}
	if err := source.handoffAsync(ctx, requestB, 5*time.Second); err != nil {
		t.Fatalf("independent token blocked behind A: %v", err)
	}
	r := record(24, "write-while-A-down")
	if _, err := b.Store.Write(r.ID, r); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Store.Write("stale-B", r); err == nil {
		t.Fatal("B acquired a second writer")
	}
	// Completing B must not thaw A, whose unavailable receiver still has no ACK.
	if _, err := source.Store.Write("must-stay-fenced", record(160, "late-A")); err == nil {
		t.Fatal("unrelated completion thawed A")
	}
	source.Close() // cancel the partitioned job before fixture closes the store
}

func TestTokenGateCancellationDoesNotDeleteAnotherHoldersGate(t *testing.T) {
	var gates rightGates
	token := causal.Hash("right")
	release, err := gates.acquire(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		unlock, err := gates.acquire(ctx, token)
		if err == nil {
			unlock()
		}
		waiting <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		gates.mu.Lock()
		refs := gates.entries[token].refs
		gates.mu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter did not register")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not cancel")
	}
	gates.mu.Lock()
	refs := gates.entries[token].refs
	gates.mu.Unlock()
	if refs != 1 {
		t.Fatalf("holder refs=%d", refs)
	}
	release()
	gates.mu.Lock()
	remaining := len(gates.entries)
	gates.mu.Unlock()
	if remaining != 0 {
		t.Fatal("unused token gate retained")
	}
}

func TestOrphanCompletionDropsItsProgressAndOpenMetric(t *testing.T) {
	net, source, target := streamFixture(t, 8)
	ctx := context.Background()
	state, _ := source.Store.CausalState()
	failed := false
	source.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		out, err := net.Exchange(ctx, host, r)
		if r.Kind == "install-zone" && !failed && err == nil {
			failed = true
			// Stop this attempt so the durable orphan is recovered explicitly.
			return out, errors.Join(storage.ErrClosed, errors.New("lost ACK"))
		}
		return out, err
	})
	request := Request{Kind: "handoff", Input: state.Genesis.Right().ID, Target: target.Self, Region: tree.Region{}}
	if err := source.handoff(ctx, request); err == nil {
		t.Fatal("lost ACK was not injected")
	}
	before := metrics.Snapshot()["handoffs"].(map[string]any)["open"].(map[string]any)["count"].(int)
	pending, _ := source.Store.CausalState()
	var old causal.TransferID
	for id, tr := range pending.Transfers {
		if tr.Phase != causal.TransferInstalled && tr.Cession.Target.Owner == target.Self.Owner {
			old = id
		}
	}
	if !old.Valid() {
		t.Fatal("lost ACK did not retain actual source operation")
	}
	request.Operation = old
	if err := source.handoff(ctx, request); err != nil {
		t.Fatalf("orphan recovery: %v", err)
	}
	source.jobs.mu.Lock()
	_, retained := source.jobs.known[old]
	source.jobs.mu.Unlock()
	if retained {
		t.Fatal("completed orphan retained its progress map")
	}
	after := metrics.Snapshot()["handoffs"].(map[string]any)["open"].(map[string]any)["count"].(int)
	if after != before-1 {
		t.Fatalf("completed orphan metric retained: before=%d after=%d", before, after)
	}
}

func TestSourceResidualProgressesBeforeParentInstallACK(t *testing.T) {
	net, nodes := meshFixture(t, 3)
	source, a, b := nodes[0], nodes[1], nodes[2]
	b.VNode.ID = tree.ID{17, 1}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blocked := make(chan struct{}, 1)
	source.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if host == a.Self.Host && r.Kind == "install" {
			select {
			case blocked <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return Response{}, ctx.Err()
		}
		return net.Exchange(ctx, host, r)
	})
	state, _ := source.Store.CausalState()
	parent := Request{Kind: "handoff", Input: state.Genesis.Right().ID, Target: a.Self, Region: tree.Region{Mask: tree.ID{192}, Value: tree.ID{64}}}
	_ = source.handoffAsync(ctx, parent, time.Millisecond)
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("parent did not emit its certificate")
	}
	state, _ = source.Store.CausalState()
	var residual causal.Digest
	for _, right := range state.Permissions.Native {
		if right.Region.Contains(tree.ID{24, 1}) {
			residual = right.ID
		}
	}
	if residual == (causal.Digest{}) || residual == parent.Input {
		t.Fatal("no newly installed source residual")
	}
	address := tree.ID{24, 1}
	r := storage.Record{Collection: "Demo000000000000", Location: tree.BASE64.Encode(address[:]), ID: "residual-write"}
	if _, err := source.Store.Write(r.ID, r); err != nil {
		t.Fatal(err)
	}
	next := Request{Kind: "handoff", Input: residual, Target: b.Self, TargetVNode: b.VNode, Region: tree.Region{Mask: tree.ID{208}, Value: tree.ID{16}}}
	if err := source.handoffAsync(ctx, next, 5*time.Second); err != nil {
		t.Fatalf("residual waited for its parent's ACK: %v", err)
	}
	r.ID = "receiver-write-before-parent-ack"
	if _, err := b.Store.Write(r.ID, r); err != nil {
		t.Fatal(err)
	}
	frozen, err := source.Store.FrozenOutgoing()
	if err != nil || len(frozen) != 1 {
		t.Fatalf("parent fence changed: %v %v", frozen, err)
	}
	source.Close()
}
