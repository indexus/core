package mesh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// A transient zero-writer interval is legal after the source's irreversible
// cession and before installation. The same bounded predecessor/successor
// path must work independently of unrelated membership size.
func TestWriteDiscoveryDoesNotRecursivelyFanOutAcrossMembers(t *testing.T) {
	for _, count := range []int{8, 64} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			nodes := make([]*Service, count)
			byHost := make(map[string]*Service, count)
			var calls atomic.Int64
			limit := int64(20)
			transport := transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
				if r.Kind != "write" || r.Membership != nil {
					t.Error("data discovery carried membership control")
				}
				if calls.Add(1) > limit {
					// Keep the deliberately incorrect algorithm from creating a
					// factorial test workload. The count assertion still fails.
					return Response{}, errors.New("discovery work exceeded its test budget")
				}
				out, err := byHost[host].Handle(ctx, r)
				if out.Membership != nil {
					t.Error("data response carried membership control")
				}
				return out, err
			})
			var members []causal.Member
			for i := range nodes {
				name := fmt.Sprintf("member-%02d", i)
				disk, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "store"), name, "discovery", 1<<26, testStore)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = disk.Close() })
				endpoint, err := disk.Endpoint(name)
				if err != nil {
					t.Fatal(err)
				}
				node := &Service{Store: disk, Self: endpoint, VNode: tree.VNodeDescriptor{Name: name, ID: tree.ID{byte(4*i + 1), 1}}, Peers: transport}
				t.Cleanup(node.Close)
				nodes[i], byHost[name] = node, node
				members = append(members, causal.Member{VNode: node.VNode, Endpoint: endpoint})
			}
			genesis := causal.Genesis{Cluster: "discovery", Holder: nodes[0].Self}
			for _, node := range nodes {
				if err := node.Store.InitCausal(nodes[0].VNode, genesis); err != nil {
					t.Fatal(err)
				}
				// This is a compatible current snapshot: other identities may
				// have ceded their previous rights back to the source. No
				// membership record itself supplies any write permission.
				if err := node.Store.MergeMembership(causal.MembershipDelta{Members: members}); err != nil {
					t.Fatal(err)
				}
			}
			source, receiver, entry := nodes[0], nodes[count-1], nodes[1]
			op := confirmedTransfer(t, source, receiver, genesis.Right().ID, tree.Region{})
			if _, err := source.Store.ForceFreezeRegion(op.Cession.ID); err != nil {
				t.Fatal(err)
			}
			offer, err := source.Store.SealCausalRegion(op.Cession.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := receiver.Store.StageCausal(offer); err != nil {
				t.Fatal(err)
			}
			cession, err := source.Store.EmitCausal(op.Cession.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			record := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "after-gap"}
			out, err := entry.Write(ctx, "same-request", record)
			if err == nil || out.Applied || ctx.Err() != nil {
				t.Fatalf("zero-writer gap must refuse promptly without ACK: %+v %v", out, err)
			}
			if got := calls.Load(); got > limit {
				t.Fatalf("%d members caused %d data RPCs, limit %d", count, got, limit)
			}
			t.Logf("members=%d zero-writer RPCs=%d", count, calls.Load())
			if err := receiver.Store.InstallCausal(cession); err != nil {
				t.Fatal(err)
			}
			calls.Store(0)
			out, err = entry.Write(ctx, "same-request", record)
			if err != nil || !out.Applied || out.AppliedBy.Owner != receiver.Self.Owner {
				t.Fatalf("installation did not resume writes: %+v %v", out, err)
			}
		})
	}
}

func TestCAP2SaturatedOwnerStillForwardsTransferredZones(t *testing.T) {
	_, nodes := meshFixture(t, 2)
	source, receiver := nodes[0], nodes[1]
	local := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "local"}
	remote := storage.Record{Collection: local.Collection, Location: "bb", ID: "remote"}
	zone, err := tree.NewZoneKey(remote.Collection, remote.Location)
	if err != nil {
		t.Fatal(err)
	}
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	op := confirmedTransfer(t, source, receiver, state.Genesis.Right().ID, tree.Point(zone.ID))
	if _, err := source.Store.ForceFreezeRegion(op.Cession.ID); err != nil {
		t.Fatal(err)
	}
	sealed, err := source.Store.SealCausalRegion(op.Cession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.Store.StageCausal(sealed); err != nil {
		t.Fatal(err)
	}
	cession, err := source.Store.EmitCausal(op.Cession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.Store.InstallCausal(cession); err != nil {
		t.Fatal(err)
	}
	source.WriteRate = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := source.Write(ctx, "consume-local-budget", local); err != nil || !out.Applied || out.AppliedBy.Owner != source.Self.Owner {
		t.Fatalf("local admission did not consume its token: %+v %v", out, err)
	}
	if out, err := source.Write(ctx, "forward-with-empty-local-budget", remote); err != nil || !out.Applied || out.AppliedBy.Owner != receiver.Self.Owner {
		t.Fatalf("local saturation blocked the transferred zone: %+v %v", out, err)
	}
	local.ID = "still-capped"
	if _, err := source.Write(ctx, "second-local-write", local); !errors.Is(err, storage.ErrWriteOverload) {
		t.Fatalf("forwarding refunded an already consumed local token: %v", err)
	}
}
