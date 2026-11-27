package mesh

import (
	"context"
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"sync/atomic"
	"testing"
	"time"
)

// An unavailable incoming source must not suppress an independent durable decision.
func TestIncomingFailureDoesNotStarveIndependentOutgoing(t *testing.T) {
	net, nodes := meshFixture(t, 3)
	a, b, c := nodes[0], nodes[1], nodes[2]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, e := a.Store.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	if e = a.handoff(ctx, Request{Kind: "handoff", Input: state.Genesis.Right().ID, Target: b.Self, Region: tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}}); e != nil {
		t.Fatal(e)
	}
	sa, e := a.Store.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	sb, e := b.Store.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	var ar, br causal.Right
	for _, r := range sa.Permissions.Native {
		ar = r
		break
	}
	for _, r := range sb.Permissions.Native {
		br = r
		break
	}
	if !ar.Valid() || !br.Valid() {
		t.Fatal("missing disjoint native rights")
	}
	confirmedTransfer(t, a, b, ar.ID, ar.Region)
	outgoing := confirmedTransfer(t, b, c, br.ID, br.Region)
	// Lose the regional installation reply after C has committed it. B must
	// retain its fence and later confirm this exact decision, not choose again.
	lost := errors.New("lost installation reply")
	var dropped atomic.Bool
	net.setAfter(func(r Request) error {
		if r.Kind == "install" && r.Cession != nil && r.Cession.Target.Owner == c.Self.Owner && dropped.CompareAndSwap(false, true) {
			return lost
		}
		return nil
	})
	if err := b.handoff(ctx, Request{Kind: "handoff", Operation: outgoing.Cession.ID}); !errors.Is(err, lost) {
		t.Fatalf("lost reply not exercised: %v", err)
	}
	net.setAfter(nil)
	record := storage.Record{Collection: tree.EncodeName(br.Region.Value), Location: "@", ID: "receiver-ACK"}
	if _, err := c.Store.Write("receiver-ACK", record); err != nil {
		t.Fatal(err)
	}
	incomingFailure := errors.New("diagnostic: incoming source unavailable")
	var outgoingCalls atomic.Int64
	b.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if host == a.Self.Host && r.Kind == "handoff" {
			return Response{}, incomingFailure
		}
		if host == c.Self.Host {
			outgoingCalls.Add(1)
		}
		return net.Exchange(ctx, host, r)
	})
	e = b.Resume(ctx)
	if !errors.Is(e, incomingFailure) {
		t.Fatalf("incoming failure absent: %v", e)
	}
	if outgoingCalls.Load() == 0 {
		t.Fatal("incoming failure starved independent outgoing recovery")
	}
	after, e := b.Store.CausalState()
	if e != nil {
		t.Fatal(e)
	}
	if after.Transfers[outgoing.Cession.ID].Phase != causal.TransferInstalled {
		t.Fatal("outgoing did not finish despite independent healthy receiver")
	}

	z, err := tree.NewZoneKey(record.Collection, record.Location)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := c.Store.Snapshot(z)
	if err != nil || len(snapshot.Records) != 1 {
		t.Fatalf("recovery lost receiver ACK: %+v %v", snapshot, err)
	}
	if _, err := b.Store.Write("old-source", record); err == nil {
		t.Fatal("recovery thawed the old writer")
	}
}

func TestBusyDonorDoesNotReceiveAProposalPerAdvertisedRight(t *testing.T) {
	net, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	// Produce a real fragmented frontier. A busy donor cannot work on any of
	// these pieces now; enumerating all of them into proposals is wasted work.
	for i := 64; i < 96; i++ {
		id := tree.ID{byte(i), 1}
		state, err := source.Store.CausalState()
		if err != nil {
			t.Fatal(err)
		}
		right, ok := state.Permissions.At(id)
		if !ok {
			t.Fatal("fixture lost native right")
		}
		if _, err := source.Store.BeginCausalPiece(context.Background(), right.ID, target.Self, tree.Point(id)); err != nil {
			t.Fatal(err)
		}
	}
	source.jobs.jobs = map[string]*handoffJob{
		"busy-a": {done: make(chan struct{})},
		"busy-b": {done: make(chan struct{})},
	}
	var proposals atomic.Int64
	net.setBefore(func(r Request) error {
		if r.Kind == "handoff" {
			proposals.Add(1)
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := target.Join(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy donor: %v", err)
	}
	if n := proposals.Load(); n == 0 || n > 8 {
		t.Fatalf("busy donor got %d proposals in 500ms", n)
	}
}
