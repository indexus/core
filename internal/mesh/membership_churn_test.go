package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// Compare two transports on the same sequential schedule. Full mode drops the previous
// prepared revision before a changed snapshot is exported, disabling the
// sparse codec while retaining the same receipt and repair logic. Lengths include the JSON reply wrapper, not HTTP headers.
type churnTraffic struct {
	full, delta, reference, retries, needBody, exchanges int
	fullBytes, deltaBytes, referenceBytes                int
}

func (c *churnTraffic) count(packet MembershipPacket, bytes int) {
	switch {
	case packet.Body == nil:
		c.reference++
		c.referenceBytes += bytes
	case packet.Base != (causal.Digest{}):
		c.delta++
		c.deltaBytes += bytes
	default:
		c.full++
		c.fullBytes += bytes
	}
}

func (c churnTraffic) bytes() int { return c.fullBytes + c.deltaBytes + c.referenceBytes }

type churnPeer struct {
	member causal.Member
	wire   *membershipWire
	state  *membershipWireState
}

func churnExchange(t *testing.T, src, dst *churnPeer, traffic *churnTraffic) {
	t.Helper()
	encodedSize := func(packet any) int {
		b, err := json.Marshal(packet)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	traffic.exchanges++
	calls := 0
	err := src.wire.exchange(context.Background(), dst.member.Endpoint.Host, func(ctx context.Context, _ string, p MembershipPacket) (MembershipReply, error) {
		calls++
		traffic.count(p, encodedSize(p))
		out, err := dst.wire.receive(ctx, src.member.Endpoint.Host, p)
		if err != nil {
			return out, err
		}
		if out.NeedBody {
			traffic.needBody++
		}
		traffic.count(out.Packet, encodedSize(out))
		return out, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	traffic.retries += calls - 1
	if calls > 2 {
		t.Fatalf("unbounded repair: %d calls", calls)
	}
}

func runMembershipChurn(t *testing.T, missedRounds int, full bool) (bootstrap, gossip churnTraffic) {
	t.Helper()
	const existing, arrivals = 8, 8
	var peers []*churnPeer
	byHost := map[string]*churnPeer{}
	add := func(index int) *churnPeer {
		w, s := membershipWireFixture(t, "")
		if full {
			snapshot := w.snapshot
			w.snapshot = func() (localMembership, error) {
				local, err := snapshot()
				if cached := w.prepared.Load(); cached != nil && cached.key != local.key {
					w.prepared.Store(nil)
				}
				return local, err
			}
		}
		m := incrementalTestMember(index)
		if index == 0 {
			m = causal.Member{VNode: s.root, Endpoint: s.genesis.Holder}
		}
		p := &churnPeer{m, w, s}
		peers = append(peers, p)
		byHost[m.Endpoint.Host] = p
		return p
	}
	announce := func(p *churnPeer, members []causal.Member) {
		if err := p.state.view.Merge(causal.MembershipDelta{Members: members}); err != nil {
			t.Fatal(err)
		}
	}
	var initial []causal.Member
	for i := 0; i < existing; i++ {
		initial = append(initial, add(i).member)
	}
	for _, p := range peers {
		announce(p, initial)
	}
	round := uint64(0)
	tick := func(isolate bool, traffic *churnTraffic) {
		for _, src := range peers {
			if isolate && src == peers[1] {
				continue
			}
			var contacts []GossipMember
			for _, m := range src.state.view.Export().Members {
				contacts = append(contacts, GossipMember{ID: m.VNode.ID, Host: m.Endpoint.Host})
			}
			for _, host := range GossipPeers(src.member.VNode.ID, src.member.Endpoint.Host, contacts, nil, round) {
				dst := byHost[host]
				if isolate && dst == peers[1] {
					continue
				}
				churnExchange(t, src, dst, traffic)
			}
		}
		round++
	}
	// Establish mature peer receipts before injecting cold joins. The first
	// contact for each new vnode is made only with its designated introducer.
	var warm churnTraffic
	for i := 0; i < existing; i++ {
		tick(false, &warm)
	}
	for i := 0; i < arrivals; i += 2 {
		for j := 0; j < 2; j++ {
			joining := add(existing + i + j)
			announce(joining, []causal.Member{joining.member})
			introducer := peers[2+(i+j)%(existing-2)]
			churnExchange(t, joining, introducer, &bootstrap)
		}
		tick(i/2 < missedRounds, &gossip)
	}
	for repair := 0; repair <= 8; repair++ {
		converged := true
		for _, p := range peers {
			if p.state.view.Digest() != peers[0].state.view.Digest() || len(p.state.view.Export().Members) != len(peers) {
				converged = false
				break
			}
		}
		if converged {
			t.Logf("missed=%d full-mode=%t repair rounds=%d, bootstrap=%d B, gossip=%d B (full %d/%d B, delta %d/%d B, ref %d/%d B), %d exchanges, %d retries, %d NeedBody", missedRounds, full, repair, bootstrap.bytes(), gossip.bytes(), gossip.full, gossip.fullBytes, gossip.delta, gossip.deltaBytes, gossip.reference, gossip.referenceBytes, gossip.exchanges, gossip.retries, gossip.needBody)
			return
		}
		tick(false, &gossip)
	}
	t.Fatal("peers failed to converge after links recovered")
	return
}

func TestMembershipChurnAcrossNeighborGaps(t *testing.T) {
	for _, gap := range []int{0, 2, 4} {
		t.Run(fmt.Sprint(gap), func(t *testing.T) {
			start, compact := runMembershipChurn(t, gap, false)
			fullStart, control := runMembershipChurn(t, gap, true)
			if start.full == 0 || compact.delta == 0 || control.delta != 0 {
				t.Fatal("scenario missed cold bootstrap or transport comparison")
			}
			if control.exchanges != compact.exchanges || start.exchanges != fullStart.exchanges {
				t.Fatal("control and sparse runs took different schedules")
			}
			// The old speculative repair repeated roughly one exchange in three
			// and increased total bytes by 21% on this deterministic topology.
			// Permit small repair overhead relative to full-only on tiny views.
			ratio := float64(compact.bytes()+start.bytes()) / float64(control.bytes()+fullStart.bytes())
			t.Logf("sparse/full-only bytes including bootstrap: %.3f", ratio)
			if ratio > 1.05 || 5*compact.retries > compact.exchanges {
				t.Errorf("repair overhead: %.3f x full-only bytes, %d retries for %d exchanges", ratio, compact.retries, compact.exchanges)
			}
		})
	}
}
