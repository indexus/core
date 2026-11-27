package mesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/replication/convergent"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// TestREPL4_FallbackWriteRepairAndConvergence exercises partition fallback
// writes on a non-owner replica, deterministic merge on repair via
// convergent.Wins, and shadow catch-up after repair.
func TestREPL4_FallbackWriteRepairAndConvergence(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 3)
	ctx := context.Background()
	for _, n := range nodes[1:] {
		if err := n.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	hosts := make([]string, len(nodes))
	for i, n := range nodes {
		hosts[i] = n.Self.Host
	}
	for _, n := range nodes {
		if err := n.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
	}

	// Owner writes k1 and syncs shadow copies to R(z).
	r1 := storage.Record{Collection: "Collection000000", Location: "aa", ID: "k1", Metrics: []float64{10}}
	if _, err := nodes[0].Write(ctx, "req-owner-1", r1); err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if err := n.Replicate(ctx); err != nil {
			t.Fatal(err)
		}
	}

	z, err := tree.NewZoneKey("Collection000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	routing, err := nodes[0].Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	ownerHolder, err := routing.Route(z.ID)
	if err != nil {
		t.Fatal(err)
	}
	var replica *Service
	for _, m := range replicas(routing, z, machineOf) {
		if m != ownerHolder {
			replica = net.nodes[m.Host]
			break
		}
	}
	if replica == nil {
		t.Fatal("expected a non-owner replica in R(z)")
	}

	// Install a controllable clock on replica so we can verify the suspicion gate.
	clock := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	replica.Now = func() time.Time { return clock }
	ownerSvc := net.nodes[ownerHolder.Host]
	if err := ownerSvc.SendHeartbeats(ctx); err != nil {
		t.Fatal(err)
	}
	if replica.OwnerSuspected(ownerHolder) {
		t.Fatal("owner should not be suspected right after heartbeat")
	}

	// While owner is healthy, fallback writes are redirected.
	r1Fb := storage.Record{Collection: "Collection000000", Location: "aa", ID: "k1", Metrics: []float64{99}}
	if _, err := replica.Handle(ctx, Request{Kind: KindFallbackWrite, Owner: ownerHolder, RequestID: "fb-early", Record: &r1Fb}); !errors.Is(err, convergent.ErrRedirect) {
		t.Fatalf("fallback write before suspicion: got %v, want convergent.ErrRedirect", err)
	}

	// Partition: silence exceeds SuspectTimeout -> replica enters fallback.
	clock = clock.Add(4 * time.Second)
	if !replica.OwnerSuspected(ownerHolder) {
		t.Fatal("owner should be suspected after SuspectTimeout")
	}
	res1, err := replica.Handle(ctx, Request{Kind: KindFallbackWrite, Owner: ownerHolder, RequestID: "fb-1", Record: &r1Fb})
	if err != nil || res1.FallbackEntry == nil || res1.FallbackEntry.Ver != 2 {
		t.Fatalf("fallback write k1: res=%+v err=%v", res1, err)
	}
	r2Fb := storage.Record{Collection: "Collection000000", Location: "aa", ID: "k2", Metrics: []float64{7}}
	if _, err := replica.Handle(ctx, Request{Kind: KindFallbackWrite, Owner: ownerHolder, RequestID: "fb-2", Record: &r2Fb}); err != nil {
		t.Fatal(err)
	}

	// Degraded read returns Degraded=true and sees fallback overlay.
	dr, err := replica.Handle(ctx, Request{Kind: KindDegradedRead, Owner: ownerHolder, Zone: z, RecordID: "k1"})
	if err != nil || !dr.Degraded || dr.Record == nil || dr.Record.Metrics[0] != 99 {
		t.Fatalf("degraded read k1: res=%+v err=%v", dr, err)
	}

	// Heal: replica discovers pending fallback zones via its local P2P routing
	// view, repairs onto owner, clears fallback log, and shadow catches up.
	if err := replica.RepairAllFallback(ctx); err != nil {
		t.Fatal(err)
	}
	if left, err := replica.Store.FallbackEntries(z); err != nil || len(left) != 0 {
		t.Fatalf("fallback entries after repair: len=%d err=%v", len(left), err)
	}
	if zones, err := replica.Store.FallbackZones(); err != nil || len(zones) != 0 {
		t.Fatalf("fallback zones after repair: %v err=%v", zones, err)
	}
	snap, err := ownerSvc.Store.Snapshot(z)
	if err != nil || len(snap.Records) != 2 {
		t.Fatalf("owner snapshot after repair: %+v err=%v", snap, err)
	}
	vals := map[string]float64{}
	for _, r := range snap.Records {
		vals[r.ID] = r.Metrics[0]
	}
	if vals["k1"] != 99 || vals["k2"] != 7 {
		t.Fatalf("merged values = %v, want k1=99 k2=7", vals)
	}

	if err := ownerSvc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if replica.OwnerSuspected(ownerHolder) {
		t.Fatal("owner should no longer be suspected after Replicate")
	}
	if seq, ok, err := replica.Store.ShadowSeq(ownerHolder, z); err != nil || !ok || seq != snap.Seq {
		t.Fatalf("shadow seq after repair = (%d, %v, %v), want %d", seq, ok, err, snap.Seq)
	}
}

// TestREPL4_TwoReplicasRepairConvergeInEitherOrder verifies that two replicas
// accepting conflicting fallback writes during a partition converge to the
// exact same authoritative state on the owner regardless of repair order.
func TestREPL4_TwoReplicasRepairConvergeInEitherOrder(t *testing.T) {
	t.Parallel()
	run := func(reverseOrder bool) map[string]float64 {
		net, nodes := meshFixture(t, 3)
		ctx := context.Background()
		for _, n := range nodes[1:] {
			if err := n.Join(ctx); err != nil {
				t.Fatal(err)
			}
		}
		hosts := make([]string, len(nodes))
		for i, n := range nodes {
			hosts[i] = n.Self.Host
		}
		for _, n := range nodes {
			if err := n.Sync(ctx, hosts); err != nil {
				t.Fatal(err)
			}
		}
		rInit := storage.Record{Collection: "Collection000000", Location: "aa", ID: "k1", Metrics: []float64{1}}
		if _, err := nodes[0].Write(ctx, "init-1", rInit); err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if err := n.Replicate(ctx); err != nil {
				t.Fatal(err)
			}
		}
		z, _ := tree.NewZoneKey("Collection000000", "aa")
		routing, _ := nodes[0].Store.CausalRouting()
		ownerHolder, _ := routing.Route(z.ID)
		var others []*Service
		for _, m := range replicas(routing, z, machineOf) {
			if m != ownerHolder {
				others = append(others, net.nodes[m.Host])
			}
		}
		if len(others) < 2 {
			t.Fatalf("expected 2 non-owner replicas in R(z), got %d", len(others))
		}
		// Simulate partition exceeding SuspectTimeout on both replicas.
		future := time.Now().Add(10 * time.Second)
		for _, rep := range others {
			rep.Now = func() time.Time { return future }
		}
		rB1 := storage.Record{Collection: "Collection000000", Location: "aa", ID: "k1", Metrics: []float64{111}}
		rB2 := storage.Record{Collection: "Collection000000", Location: "aa", ID: "k2", Metrics: []float64{222}}
		rC1 := storage.Record{Collection: "Collection000000", Location: "aa", ID: "k1", Metrics: []float64{333}}
		if _, err := others[0].Handle(ctx, Request{Kind: KindFallbackWrite, Owner: ownerHolder, RequestID: "b-1", Record: &rB1}); err != nil {
			t.Fatal(err)
		}
		if _, err := others[0].Handle(ctx, Request{Kind: KindFallbackWrite, Owner: ownerHolder, RequestID: "b-2", Record: &rB2}); err != nil {
			t.Fatal(err)
		}
		if _, err := others[1].Handle(ctx, Request{Kind: KindFallbackWrite, Owner: ownerHolder, RequestID: "c-1", Record: &rC1}); err != nil {
			t.Fatal(err)
		}
		first, second := others[0], others[1]
		if reverseOrder {
			first, second = second, first
		}
		if err := first.RepairFallback(ctx, ownerHolder, z); err != nil {
			t.Fatal(err)
		}
		if err := second.RepairFallback(ctx, ownerHolder, z); err != nil {
			t.Fatal(err)
		}
		snap, err := net.nodes[ownerHolder.Host].Store.Snapshot(z)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]float64{}
		for _, r := range snap.Records {
			out[r.ID] = r.Metrics[0]
		}
		return out
	}

	ab := run(false)
	ba := run(true)
	if len(ab) != len(ba) || ab["k1"] != ba["k1"] || ab["k2"] != ba["k2"] {
		t.Fatalf("non-convergent repair: order AB=%v vs BA=%v", ab, ba)
	}
}
