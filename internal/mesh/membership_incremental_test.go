package mesh

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func incrementalTestMember(n int) causal.Member {
	name := fmt.Sprintf("member-%08d", n)
	id := sha256.Sum256([]byte(name))
	return causal.Member{
		VNode:    tree.VNodeDescriptor{Name: name, ID: tree.ID(id[:tree.Size])},
		Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: name + "-disk"}, Host: name + ":443"},
	}
}

type exchangeBytes struct {
	request, reply, calls int
}

func measureMembershipExchange(t *testing.T, a, b *membershipWire) exchangeBytes {
	t.Helper()
	var bytes exchangeBytes
	send := membershipJSONSend(t, b, nil)
	err := a.exchange(context.Background(), "b", func(ctx context.Context, host string, packet MembershipPacket) (MembershipReply, error) {
		req, err := json.Marshal(packet)
		if err != nil {
			return MembershipReply{}, err
		}
		bytes.request += len(req)
		bytes.calls++
		out, err := send(ctx, host, packet)
		if err != nil {
			return out, err
		}
		response, err := json.Marshal(out)
		bytes.reply += len(response)
		return out, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return bytes
}

// Count JSON bytes after encoding real requests and replies. Just checking the
// sender's patch size would miss a full snapshot reflected by the receiver.
func TestMembershipOneJoinWireCost(t *testing.T) {
	for _, n := range []int{64, 1024, 10000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			a, sa := membershipWireFixture(t, "")
			b, sb := membershipWireFixture(t, "")
			records := make([]causal.Member, n-1)
			for i := range records {
				records[i] = incrementalTestMember(i)
			}
			for _, s := range []*membershipWireState{sa, sb} {
				if err := s.view.Merge(causal.MembershipDelta{Members: records}); err != nil {
					t.Fatal(err)
				}
			}
			initial := measureMembershipExchange(t, a, b)
			stable := measureMembershipExchange(t, a, b)
			join := incrementalTestMember(n)
			if err := sa.view.Merge(causal.MembershipDelta{Members: []causal.Member{join}}); err != nil {
				t.Fatal(err)
			}
			update := measureMembershipExchange(t, a, b)
			t.Logf("baseline %d B, stable %d B, one join request %d B + reply %d B in %d RPC", initial.request+initial.reply, stable.request+stable.reply, update.request, update.reply, update.calls)
			if sa.view.Digest() != sb.view.Digest() || len(sb.view.Export().Members) != n+1 {
				t.Fatal("join missing after successful exchange")
			}
			// Allow both directions to carry identity, hash and receipt fields.
			if update.calls != 1 || update.request+update.reply > 4096 {
				t.Errorf("one join costs %d bytes in %d calls with %d current members", update.request+update.reply, update.calls, n)
			}
			// In a push-pull network a node can also discover its peer's newer
			// view from the reply. Exercise that direction on an established pair.
			measureMembershipExchange(t, a, b)
			if err := sb.view.Merge(causal.MembershipDelta{Members: []causal.Member{incrementalTestMember(n + 1)}}); err != nil {
				t.Fatal(err)
			}
			pull := measureMembershipExchange(t, a, b)
			t.Logf("one join observed only by responder: request %d B + reply %d B in %d RPC", pull.request, pull.reply, pull.calls)
			if sa.view.Digest() != sb.view.Digest() || len(sa.view.Export().Members) != n+2 {
				t.Fatal("responder join missing after successful exchange")
			}
			if pull.calls != 1 || pull.request+pull.reply > 4096 {
				t.Errorf("one responder join costs %d bytes in %d calls with %d current members", pull.request+pull.reply, pull.calls, n+1)
			}
		})
	}
}

func TestMembershipIncrementalDivergenceAfterRestart(t *testing.T) {
	a, sa := membershipWireFixture(t, "")
	b, sb := membershipWireFixture(t, "")
	measureMembershipExchange(t, a, b)
	measureMembershipExchange(t, a, b)
	restarted, sr := membershipWireFixture(t, "")
	sr.view = sb.view.Clone() // durable state survives, control receipts do not
	for i, s := range []*membershipWireState{sa, sr} {
		if err := s.view.Merge(causal.MembershipDelta{Members: []causal.Member{incrementalTestMember(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	measured := measureMembershipExchange(t, a, restarted)
	if sa.view.Digest() != sr.view.Digest() || len(sr.view.Export().Members) != 3 {
		t.Fatal("divergent local joins did not converge after restart")
	}
	t.Logf("restart and concurrent joins: %d B in %d RPC", measured.request+measured.reply, measured.calls)
}

func TestMembershipRepairWhenInitiatorChangesWhileReplyInFlight(t *testing.T) {
	a, sa := membershipWireFixture(t, "")
	b, sb := membershipWireFixture(t, "")
	measureMembershipExchange(t, a, b)
	measureMembershipExchange(t, a, b)
	if err := sb.view.Merge(causal.MembershipDelta{Members: []causal.Member{incrementalTestMember(1)}}); err != nil {
		t.Fatal(err)
	}
	send := membershipJSONSend(t, b, nil)
	calls := 0
	err := a.exchange(context.Background(), "b", func(ctx context.Context, peer string, p MembershipPacket) (MembershipReply, error) {
		calls++
		out, err := send(ctx, peer, p)
		if err != nil {
			return out, err
		}
		switch calls {
		case 1:
			if out.Packet.Base == (causal.Digest{}) || out.Packet.Body == nil {
				t.Fatal("expected patch against the requester's current revision")
			}
			// B has replied, but the packet has not yet reached A.
			if err := sa.view.Merge(causal.MembershipDelta{Members: []causal.Member{incrementalTestMember(2)}}); err != nil {
				t.Fatal(err)
			}
		case 2:
			if p.Known != (causal.Digest{}) || p.Base != (causal.Digest{}) || p.Body == nil || out.Packet.Base != (causal.Digest{}) || out.Packet.Body == nil {
				t.Fatal("unusable reply patch did not trigger full repair")
			}
		}
		return out, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("repair required %d calls: %v", calls, err)
	}
	if sa.view.Digest() != sb.view.Digest() || len(sa.view.Export().Members) != 3 {
		t.Fatal("one of two concurrent joins was lost")
	}
}
