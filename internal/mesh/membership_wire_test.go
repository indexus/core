package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type membershipWireState struct {
	mu      sync.Mutex
	root    tree.VNodeDescriptor
	genesis causal.Genesis
	view    *causal.Membership
	merges  int
}

func membershipWireFixture(t *testing.T, added string) (*membershipWire, *membershipWireState) {
	t.Helper()
	s := &membershipWireState{root: tree.VNodeDescriptor{Name: "root", ID: tree.ID{1}}, genesis: causal.Genesis{Cluster: "wire-test", Holder: causal.Endpoint{Owner: causal.Owner{VNode: "root", Storage: "root-disk"}, Host: "root:1"}}}
	var err error
	s.view, err = causal.NewMembership(s.root, s.genesis)
	if err != nil {
		t.Fatal(err)
	}
	if added != "" {
		member := causal.Member{VNode: tree.VNodeDescriptor{Name: added, ID: tree.ID{added[0]}}, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: added, Storage: added + "-disk"}, Host: added + ":1"}}
		if err = s.view.Merge(causal.MembershipDelta{Members: []causal.Member{member}}); err != nil {
			t.Fatal(err)
		}
	}
	w := newMembershipWire(func() (localMembership, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.view == nil {
			return localMembership{}, nil
		}
		delta := s.view.Export()
		return localMembership{membershipKey{s.root, s.genesis, s.view.Digest()}, func() causal.MembershipDelta { return delta }}, nil
	}, func(e MembershipEnvelope) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if e.Root != s.root || e.Genesis != s.genesis {
			return causal.ErrConflict
		}
		if s.view == nil {
			var err error
			s.view, err = causal.NewMembership(e.Root, e.Genesis)
			if err != nil {
				return err
			}
		}
		if err := s.view.Merge(e.Delta); err != nil {
			return err
		}
		s.merges++
		return nil
	})
	t.Cleanup(w.close)
	return w, s
}

func membershipJSONSend(t *testing.T, remote *membershipWire, packets *[]MembershipPacket) membershipSend {
	t.Helper()
	return func(ctx context.Context, _ string, p MembershipPacket) (MembershipReply, error) {
		raw, err := json.Marshal(p)
		if err != nil {
			return MembershipReply{}, err
		}
		var copy MembershipPacket
		if err = json.Unmarshal(raw, &copy); err != nil {
			return MembershipReply{}, err
		}
		if packets != nil {
			*packets = append(*packets, copy)
		}
		out, err := remote.receive(ctx, "sender", copy)
		if err != nil {
			return out, err
		}
		raw, err = json.Marshal(out)
		if err != nil {
			return MembershipReply{}, err
		}
		var reply MembershipReply
		err = json.Unmarshal(raw, &reply)
		return reply, err
	}
}

func TestMembershipWireConvergesAndUsesOneReceiptPerPeer(t *testing.T) {
	a, sa := membershipWireFixture(t, "a")
	b, sb := membershipWireFixture(t, "b")
	var packets []MembershipPacket
	send := membershipJSONSend(t, b, &packets)
	if err := a.exchange(context.Background(), "b", send); err != nil {
		t.Fatal(err)
	}
	if sa.view.Digest() != sb.view.Digest() {
		t.Fatal("routing views did not converge")
	}
	// The first response grows A's view; the second announces that union.
	if err := a.exchange(context.Background(), "b", send); err != nil {
		t.Fatal(err)
	}
	if err := a.exchange(context.Background(), "b", send); err != nil {
		t.Fatal(err)
	}
	if packets[0].Body == nil || packets[len(packets)-1].Body != nil {
		t.Fatal("full body was not replaced by a receipt")
	}
	if len(a.peers) != 1 || len(b.peers) != 1 {
		t.Fatal("kept version history instead of one peer receipt")
	}
	if len(sa.view.Export().Members) != 3 {
		t.Fatal("membership missing a vnode")
	}
}

func TestMembershipWireLostReplyAndReceiverRestart(t *testing.T) {
	a, _ := membershipWireFixture(t, "a")
	b, _ := membershipWireFixture(t, "")
	var packets []MembershipPacket
	base := membershipJSONSend(t, b, &packets)
	lost := errors.New("reply lost after merge")
	err := a.exchange(context.Background(), "b", func(ctx context.Context, host string, p MembershipPacket) (MembershipReply, error) {
		if _, err := base(ctx, host, p); err != nil {
			return MembershipReply{}, err
		}
		return MembershipReply{}, lost
	})
	if !errors.Is(err, lost) {
		t.Fatal(err)
	}
	if a.receipt("b").sent != (causal.Digest{}) {
		t.Fatal("lost reply created a receipt")
	}
	if err = a.exchange(context.Background(), "b", base); err != nil {
		t.Fatal(err)
	}
	if packets[0].Body == nil || packets[1].Body == nil {
		t.Fatal("lost reply did not resend full membership")
	}
	// Recreate the wire process: durable membership remains, volatile receipt is lost.
	restarted, _ := membershipWireFixture(t, "")
	packets = nil
	if err = a.exchange(context.Background(), "b", membershipJSONSend(t, restarted, &packets)); err != nil {
		t.Fatal(err)
	}
	if len(packets) != 2 || packets[0].Body != nil || packets[1].Body == nil {
		t.Fatalf("restart should request precisely one full retry: %+v", packets)
	}
}

func TestMembershipWireBootstrapsWithoutLocalView(t *testing.T) {
	a, sa := membershipWireFixture(t, "")
	sa.view = nil
	b, sb := membershipWireFixture(t, "b")
	if err := a.exchange(context.Background(), "b", membershipJSONSend(t, b, nil)); err != nil {
		t.Fatal(err)
	}
	if sa.view == nil || sa.view.Digest() != sb.view.Digest() {
		t.Fatal("bootstrap did not obtain routing view")
	}
}

func TestMembershipWireRejectsCorruptionAndDetachesBodies(t *testing.T) {
	a, sa := membershipWireFixture(t, "a")
	b, sb := membershipWireFixture(t, "")
	p, err := a.packet(causal.Digest{}, true)
	if err != nil {
		t.Fatal(err)
	}
	p.Body.Delta.Members[0].Endpoint.Host = "tampered"
	if _, err = b.receive(context.Background(), "a", p); !errors.Is(err, causal.ErrConflict) {
		t.Fatal(err)
	}
	if sb.merges != 0 {
		t.Fatal("corrupt membership reached merger")
	}
	if sa.view.Export().Members[0].Endpoint.Host == "tampered" {
		t.Fatal("packet changed source routing view")
	}
	p, err = a.packet(causal.Digest{}, true)
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.receive(context.Background(), "a", p)
	if err != nil {
		t.Fatal(err)
	}
	out.Packet.Body.Delta.Members[0].Endpoint.Host = "reply-mutated"
	if sb.view.Export().Members[0].Endpoint.Host == "reply-mutated" {
		t.Fatal("reply changed receiver routing view")
	}
	bad := p
	bad.Body = nil
	bad.Version = causal.Hash("missing")
	out, err = b.receive(context.Background(), "a", bad)
	if err != nil || !out.NeedBody || out.Accepted != (causal.Digest{}) {
		t.Fatalf("missing reference accepted: %+v %v", out, err)
	}
}

func TestMembershipWireBoundsPeerReceiptsAndRetries(t *testing.T) {
	w, _ := membershipWireFixture(t, "")
	for i := 0; i < membershipPeerLimit+20; i++ {
		w.remember(fmt.Sprint(i), causal.Hash(i), causal.Digest{})
	}
	if len(w.peers) != membershipPeerLimit || len(w.order) != membershipPeerLimit {
		t.Fatal("peer receipts unbounded")
	}
	if _, ok := w.peers["0"]; ok {
		t.Fatal("old receipt not evicted")
	}
	calls := 0
	err := w.exchange(context.Background(), "missing", func(context.Context, string, MembershipPacket) (MembershipReply, error) {
		calls++
		return MembershipReply{NeedBody: true}, nil
	})
	if !errors.Is(err, causal.ErrDependency) || calls != 1 {
		t.Fatalf("full body refusal retried: %d %v", calls, err)
	}
}

func TestMembershipWireScheduleNeverWaitsAndClosesFlights(t *testing.T) {
	w, _ := membershipWireFixture(t, "")
	started := make(chan struct{}, membershipSyncLimit)
	var concurrent, maxConcurrent atomic.Int32
	send := func(ctx context.Context, _ string, _ MembershipPacket) (MembershipReply, error) {
		n := concurrent.Add(1)
		defer concurrent.Add(-1)
		for old := maxConcurrent.Load(); n > old && !maxConcurrent.CompareAndSwap(old, n); old = maxConcurrent.Load() {
		}
		started <- struct{}{}
		<-ctx.Done()
		return MembershipReply{}, ctx.Err()
	}
	for i := 0; i < membershipSyncLimit; i++ {
		if !w.schedule(fmt.Sprint(i), send) {
			t.Fatal("initial flight refused")
		}
	}
	for i := 0; i < membershipSyncLimit; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("flight did not start")
		}
	}
	returned := make(chan bool, 1)
	go func() { returned <- w.schedule("overflow", send) }()
	select {
	case ok := <-returned:
		if ok {
			t.Fatal("overflow accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler waited on network")
	}
	if !w.schedule("0", send) {
		t.Fatal("same-peer hint not deduplicated")
	}
	w.close()
	if concurrent.Load() != 0 || len(w.pending) != 0 || maxConcurrent.Load() > membershipSyncLimit {
		t.Fatal("flight budget/cleanup violated")
	}
	if w.schedule("after-close", send) {
		t.Fatal("closed scheduler accepted work")
	}
}

func TestMembershipWireConcurrentControlMerges(t *testing.T) {
	a, _ := membershipWireFixture(t, "a")
	b, sb := membershipWireFixture(t, "")
	p, err := a.packet(causal.Digest{}, true)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			if _, err := b.receive(context.Background(), "a", p); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if sb.merges != 1 {
		t.Fatalf("same peer/version merged %d times", sb.merges)
	}
}

func TestReferenceRoundDoesNotExportMembership(t *testing.T) {
	a, state := membershipWireFixture(t, "a")
	member := state.view
	key := membershipKey{state.root, state.genesis, member.Digest()}
	exports := 0
	a.snapshot = func() (localMembership, error) {
		return localMembership{key, func() causal.MembershipDelta { exports++; return member.Export() }}, nil
	}
	first, err := a.packet(causal.Digest{}, true)
	if err != nil || first.Body == nil || first.Version != causal.Hash(*first.Body) {
		t.Fatalf("full packet: %v", err)
	}
	reply, err := a.receive(context.Background(), "peer", MembershipPacket{Known: first.Version})
	if err != nil || reply.Packet.Body != nil || reply.Packet.Version != first.Version || exports != 1 {
		t.Fatalf("unchanged membership exported %d times: %+v %v", exports, reply, err)
	}
}
