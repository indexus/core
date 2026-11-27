package mesh

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/testkit"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestPhysicalReceiverDelegatesFirstNativeGrainBeforeMovePublication(t *testing.T) {
	net, nodes := meshFixture(t, 3)
	root, old, third := nodes[0], nodes[1], nodes[2]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(15*time.Second))
	defer cancel()
	initial, err := root.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if err := root.handoff(ctx, Request{Input: initial.Genesis.Right().ID, Target: old.Self, TargetVNode: old.VNode, Region: tree.Region{}}); err != nil {
		t.Fatal(err)
	}
	state, err := old.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	partition, err := old.Store.BeginCausalPiece(ctx, state.Permissions.Native[0].ID, root.Self, tree.Region{Mask: tree.ID{128}})
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Store.AbortCausal(partition.Cession.ID); err != nil {
		t.Fatal(err)
	}
	if err := old.Store.FinishAbortCausal(partition.Cession.ID); err != nil {
		t.Fatal(err)
	}
	// Two real canonical zones, one in each native half, so MOVE must install
	// two independent pieces rather than publishing after a single empty offer.
	var seeds []storage.Record
	for _, high := range []byte{0, 128} {
		for i := 0; i < 256; i++ {
			row := storage.Record{Collection: "Demo000000000000", Location: tree.EncodeName(tree.ID{byte(i), 1}), ID: fmt.Sprintf("seed-%d", high)}
			z, e := tree.NewZoneKey(row.Collection, row.Location)
			if e != nil {
				t.Fatal(e)
			}
			if z.ID[0]&128 != high {
				continue
			}
			if _, e = old.Store.Write(row.ID, row); e != nil {
				t.Fatal(e)
			}
			seeds = append(seeds, row)
			break
		}
	}
	if len(seeds) != 2 {
		t.Fatal("missing seed halves")
	}
	moved := physicalDrainTarget(t, net, old, "physical-first-grain")
	var delegated atomic.Bool
	var fresh storage.Record
	net.setAfter(func(r Request) error {
		if r.Kind != "install" || r.Cession == nil || r.Cession.Target.Owner != moved.Self.Owner || !delegated.CompareAndSwap(false, true) {
			return nil
		}
		local, e := moved.Store.CausalState()
		if e != nil {
			return e
		}
		membership, e := local.MembershipReplica()
		if e != nil {
			return e
		}
		published, e := membership.Endpoint(old.VNode.Name)
		if e != nil || published.Owner != old.Self.Owner {
			return fmt.Errorf("MOVE published before remaining grain: %+v %v", published, e)
		}
		remaining, e := old.Store.CausalState()
		if e != nil || len(remaining.Permissions.Native) == 0 {
			return fmt.Errorf("no remaining physical grain: %v", e)
		}
		grain := r.Cession.Right()
		if _, ok := local.Permissions.Find(grain.ID); !ok {
			return fmt.Errorf("first grain is not native")
		}
		if e = moved.handoff(ctx, Request{Input: grain.ID, Target: third.Self, TargetVNode: third.VNode, Region: grain.Region}); e != nil {
			return e
		}
		for i := 0; i < 256; i++ {
			fresh = storage.Record{Collection: "Empty00000000000", Location: tree.EncodeName(tree.ID{byte(i), 3}), ID: "ACK-before-MOVE-published"}
			z, e := tree.NewZoneKey(fresh.Collection, fresh.Location)
			if e != nil {
				return e
			}
			if !grain.Region.Contains(z.ID) {
				continue
			}
			out, e := moved.Write(ctx, fresh.ID, fresh)
			if e != nil || out.AppliedBy.Owner != third.Self.Owner {
				return fmt.Errorf("future physical endpoint could not route delegated empty space: %+v %v", out.AppliedBy, e)
			}
			return nil
		}
		return fmt.Errorf("no address in first grain")
	})
	t.Cleanup(func() { net.setAfter(nil) })
	if err := moved.ReceiveMove(ctx, old.Self.Host); err != nil {
		t.Fatal(err)
	}
	net.setAfter(nil)
	if !delegated.Load() {
		t.Fatal("first native grain was never delegated")
	}
	net.setBlocked(old.Self.Host, true)
	if err := root.Sync(ctx, []string{moved.Self.Host, third.Self.Host}); err != nil {
		t.Fatal(err)
	}
	rows, err := root.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, snap := range rows {
		for _, row := range snap.Records {
			if !row.Deleted {
				seen[row.ID] = true
			}
		}
	}
	for _, row := range append(seeds, fresh) {
		if !seen[row.ID] {
			t.Fatalf("lost ACK after old host stopped: %s", row.ID)
		}
	}
	writers := 0
	for _, node := range []*Service{root, old, moved, third} {
		copy := fresh
		copy.ID = "single-writer-probe"
		if _, e := node.Store.Write(copy.ID, copy); e == nil {
			writers++
		}
	}
	if writers != 1 {
		t.Fatalf("delegated empty address has %d writers", writers)
	}
}
