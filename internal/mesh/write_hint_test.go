package mesh

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// hintNet delivers calls on the fixture network, records each write call as
// "host/hops", and can rewrite the AppliedBy of the next write ACK from one
// host: as if that disk had relayed the write to the disk named.
type hintNet struct {
	*network
	mu       sync.Mutex
	writes   []string
	rewrite  map[string]causal.Endpoint
	rewrites int
}

func (h *hintNet) Exchange(ctx context.Context, host string, r Request) (Response, error) {
	if r.Kind == "write" {
		h.mu.Lock()
		h.writes = append(h.writes, fmt.Sprintf("%s/%d", host, r.Hops))
		h.mu.Unlock()
	}
	out, err := h.network.Exchange(ctx, host, r)
	h.mu.Lock()
	defer h.mu.Unlock()
	if as, ok := h.rewrite[host]; ok && r.Kind == "write" && err == nil && h.rewrites > 0 {
		h.rewrites--
		out.AppliedBy = as
	}
	return out, err
}

func (h *hintNet) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.writes
	h.writes = nil
	return out
}

// TestEntryWriteFollowsAppliedByHint checks WRITE-HINT-1. node-1 is the entry
// and holds none of the zone; node-0 holds it. Once an ACK names another disk
// as the applier, the next entry write goes there directly; that disk, which
// holds nothing, relays to node-0 rather than accepting it; the ACK by node-0,
// the holder, retires the hint. A hinted disk that cannot be reached is
// forgotten, and the write reaches the holder in the same call.
func TestEntryWriteFollowsAppliedByHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture, nodes := meshFixture(t, 3)
	holder, entry, other := nodes[0], nodes[1], nodes[2]
	net := &hintNet{network: fixture}
	for _, n := range nodes {
		n.Peers = net
	}
	send := func(at *Service, id string, hops int) Response {
		t.Helper()
		r := storage.Record{Collection: "0000000000000001", Location: "abc", ID: id, Metrics: []float64{1}}
		out, err := at.Handle(ctx, Request{Kind: "write", RequestID: id, Record: &r, Hops: hops})
		if err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		return out
	}
	write := func(id string) {
		t.Helper()
		if out := send(entry, id, 0); out.AppliedBy != holder.Self {
			t.Fatalf("write %s applied by %v, want the holder %v", id, out.AppliedBy, holder.Self)
		}
	}
	want := func(step string, calls ...string) {
		t.Helper()
		if got := net.take(); !slices.Equal(got, calls) {
			t.Fatalf("%s: write calls %v, want %v", step, got, calls)
		}
	}
	h0, h2 := holder.Self.Host, other.Self.Host

	// node-0 answers as if node-2 had applied: the entry learns node-2.
	net.rewrite, net.rewrites = map[string]causal.Endpoint{h0: other.Self}, 1
	send(entry, "w1", 0)
	want("learn", h0+"/1")
	// Hinted: straight to node-2, which holds nothing and relays to node-0.
	write("w2")
	want("follow the hint", h2+"/1", h0+"/2")
	// node-0, the holder, applied: the hint is gone.
	write("w3")
	want("hint retired by the holder's ACK", h0+"/1")

	// A hint to a disk that cannot be reached: dropped, holder in the same call.
	ghost := causal.Endpoint{Owner: causal.Owner{VNode: "ghost", Storage: "ghost"}, Host: "ghost"}
	net.rewrite, net.rewrites = map[string]causal.Endpoint{h0: ghost}, 1
	send(entry, "w4", 0)
	want("learn ghost", h0+"/1")
	write("w5")
	want("unreachable hint", "ghost/1", h0+"/1")
	write("w6")
	want("hint dropped", h0+"/1")

	// node-2 learns ghost as an entry; a write that already travelled
	// (Hops > 0) ignores that hint and goes to the holder.
	net.rewrite, net.rewrites = map[string]causal.Endpoint{h0: ghost}, 1
	send(other, "w7", 0)
	want("node-2 learns ghost", h0+"/1")
	send(other, "w8", 1)
	want("relayed write", h0+"/2")
}

// A healthy local/relayed/client write must finish even while membership
// merges are stopped. Its packets contain only data, request IDs and hints.
func TestWritesNeverWaitForMembershipControl(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	writer, err := NewWriter(ctx, net, []string{nodes[0].Self.Host})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var controls atomic.Int64
	peers := transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if r.Membership != nil || r.Kind == KindMembership {
			controls.Add(1)
			return Response{}, errors.New("control unavailable")
		}
		out, e := net.Exchange(ctx, host, r)
		if out.Membership != nil {
			return out, errors.New("data response carried membership")
		}
		return out, e
	})
	for _, node := range nodes {
		node.Peers = peers
		node.membershipTransport().mergeMu.Lock()
		defer node.membershipTransport().mergeMu.Unlock()
	}
	writer.peers = peers
	writer.control.mergeMu.Lock()
	defer writer.control.mergeMu.Unlock()
	record := storage.Record{Collection: "0000000000000001", Location: "abc", ID: "direct"}
	if _, err = nodes[0].Write(ctx, record.ID, record); err != nil {
		t.Fatal(err)
	}
	record.ID = "relay"
	if _, err = nodes[1].Write(ctx, record.ID, record); err != nil {
		t.Fatal(err)
	}
	record.ID = "client"
	if _, err = writer.Write(ctx, record.ID, record); err != nil {
		t.Fatal(err)
	}
	if controls.Load() != 0 {
		t.Fatalf("writes issued %d membership requests", controls.Load())
	}
}
