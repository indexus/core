package mesh

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestIncomingReadyRoutesNewAddressToDonorWithoutMemberSweep(t *testing.T) {
	net, nodes := meshFixture(t, 4)
	source, target, entry := nodes[0], nodes[1], nodes[2]
	ctx := context.Background()
	var members []causal.Member
	for _, node := range nodes {
		members = append(members, causal.Member{VNode: node.VNode, Endpoint: node.Self})
	}
	for _, node := range nodes {
		if err := node.Store.MergeMembership(causal.MembershipDelta{Members: members}); err != nil {
			t.Fatal(err)
		}
	}
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	offer := confirmedTransfer(t, source, target, state.Permissions.Native[0].ID, tree.Region{})
	record := storage.Record{Collection: "new-collection01", Location: "aa", ID: "new-address"}
	zone, _ := tree.NewZoneKey(record.Collection, record.Location)
	holder, err := target.Store.ZoneHolder(zone)
	if err != nil || holder.Owner != source.Self.Owner {
		t.Fatalf("incoming ready must refer empty space to donor: %+v %v", holder, err)
	}
	// An entry can already know the new receiver from another ACK in the piece.
	entry.routeMu.Lock()
	entry.routes.put(zone.Key(), target.Self)
	entry.routeMu.Unlock()
	var donorHops atomic.Int64
	transport := transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if r.Kind == "write" {
			if host == nodes[3].Self.Host {
				t.Error("contacted unrelated member")
			}
			if host == source.Self.Host {
				donorHops.Store(int64(r.Hops))
			}
		}
		return net.Exchange(ctx, host, r)
	})
	entry.Peers, target.Peers = transport, transport
	began := time.Now()
	out, err := entry.Handle(ctx, Request{Kind: "write", RequestID: record.ID, Record: &record})
	if err != nil || out.AppliedBy.Owner != source.Self.Owner || donorHops.Load() != 2 {
		t.Fatalf("new address did not follow receiver's donor: %+v hops=%d err=%v", out, donorHops.Load(), err)
	}
	if time.Since(began) > 600*time.Millisecond {
		t.Fatal("new address waited for consolidation")
	}
	if err := source.handoff(ctx, Request{Kind: "handoff", Operation: offer.Cession.ID, Input: offer.Cession.Input.ID, Target: target.Self, Region: tree.Region{}}); err != nil {
		t.Fatal(err)
	}
	holder, err = target.Store.ZoneHolder(zone)
	if err != nil || holder.Owner != target.Self.Owner {
		t.Fatalf("installed receiver did not take ownership: %+v %v", holder, err)
	}
}

func TestInstallRetryRepairsLostACKAndTemporaryOutageWithoutMaintenance(t *testing.T) {
	for _, outage := range []time.Duration{0, 750 * time.Millisecond} {
		t.Run(fmt.Sprint(outage), func(t *testing.T) {
			net, source, target := streamFixture(t, 2)
			var attempts atomic.Int64
			var began time.Time
			source.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
				if r.Kind != "install-zone" {
					return net.Exchange(ctx, host, r)
				}
				if attempts.Add(1) == 1 {
					began = time.Now()
				}
				if outage > 0 && time.Since(began) < outage {
					return Response{}, fmt.Errorf("receiver unavailable")
				}
				out, err := net.Exchange(ctx, host, r)
				if outage == 0 && attempts.Load() == 1 && err == nil {
					return out, fmt.Errorf("lost install ACK")
				}
				return out, err
			})
			state, _ := source.Store.CausalState()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := source.handoff(ctx, Request{Kind: "handoff", Input: state.Genesis.Right().ID, Target: target.Self, Region: tree.Region{}})
			if err != nil {
				t.Fatal(err)
			}
			if attempts.Load() < 2 {
				t.Fatal("failure did not cause local retry")
			}
			if time.Since(began) > 2*time.Second {
				t.Fatal("recovery waited for maintenance")
			}
			states, err := source.Store.CausalState()
			if err != nil {
				t.Fatal(err)
			}
			for _, tr := range states.Transfers {
				if tr.Phase != causal.TransferInstalled {
					t.Fatalf("open transfer after recovery: %v", tr.Phase)
				}
			}
		})
	}
}

// Quiescing a selected point must let an optimistic fence finish without
// stopping an unrelated point. Every queued ACK then lands on the receiver.
func TestCutoverPreparationQuiescesOnlyItsLot(t *testing.T) {
	_, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seed := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "seed"}
	if _, err := source.Write(ctx, seed.ID, seed); err != nil {
		t.Fatal(err)
	}
	zone, _ := tree.NewZoneKey(seed.Collection, seed.Location)
	state, _ := source.Store.CausalState()
	offer := confirmedTransfer(t, source, target, state.Genesis.Right().ID, tree.Point(zone.ID))
	keys := []tree.ZoneKey{zone}
	done := source.beginCutover(keys)
	released := false
	defer func() {
		if !released {
			source.endCutover(keys, done)
		}
	}()
	results := make(chan error, 32)
	entered := make(chan struct{}, 32)
	for i := range 32 {
		go func() {
			record := seed
			record.ID = fmt.Sprintf("hot-%d", i)
			entered <- struct{}{}
			out, err := source.Handle(ctx, Request{Kind: "write", RequestID: record.ID, Record: &record})
			if err == nil && out.AppliedBy.Owner != target.Self.Owner {
				err = fmt.Errorf("queued write ACKed at source")
			}
			results <- err
		}()
	}
	for range 32 {
		<-entered
	}
	unrelated := seed
	unrelated.Location, unrelated.ID = "bb", "outside-lot"
	if _, err := source.Write(ctx, unrelated.ID, unrelated); err != nil {
		t.Fatalf("unrelated write blocked: %v", err)
	}
	snaps, _, proofs, err := source.Store.FenceZones(offer.Cession.ID, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || len(snaps[0].Records) != 1 {
		t.Fatal("write crossed preparation gate")
	}
	if err := target.Store.MountZones(offer.Cession.ID, source.Self.VNode, nil, nil, snaps, proofs); err != nil {
		t.Fatal(err)
	}
	// Installed points must receive new writes even before cutover waiters are
	// released (e.g. donor has not received the install ACK yet).
	early := seed
	early.ID = "installed-before-ack"
	if out, err := source.Handle(ctx, Request{Kind: "write", RequestID: early.ID, Record: &early}); err != nil || out.AppliedBy.Owner != target.Self.Owner {
		t.Fatalf("installed receiver waited for source bookkeeping: %+v %v", out, err)
	}
	source.endCutover(keys, done)
	released = true
	for range 32 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	snap, err := target.Store.Snapshot(zone)
	if err != nil || len(snap.Records) != 34 {
		t.Fatalf("ACKs not retained: rows=%d err=%v", len(snap.Records), err)
	}
}

func TestJoinRetriesShortTransportCutButPreservesStoppedWorker(t *testing.T) {
	for _, kind := range []string{KindRights, "handoff", "install"} {
		t.Run(kind, func(t *testing.T) {
			net, nodes := meshFixture(t, 2)
			target := nodes[1]
			var cuts atomic.Int64
			target.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
				if r.Kind == kind && cuts.Add(1) <= 2 {
					return Response{}, fmt.Errorf("short transport partition")
				}
				return net.Exchange(ctx, host, r)
			})
			nodes[0].Peers = target.Peers
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := target.Join(ctx); err != nil {
				t.Fatal(err)
			}
			if cuts.Load() < 3 {
				t.Fatal("transport retry not exercised")
			}
		})
	}
	_, nodes := meshFixture(t, 2)
	var calls atomic.Int64
	nodes[1].Peers = transportFunc(func(context.Context, string, Request) (Response, error) {
		calls.Add(1)
		return Response{}, storage.ErrClosed
	})
	if err := nodes[1].Join(context.Background()); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("stopped worker hidden: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("terminal refusal retried %d times", calls.Load())
	}
}
