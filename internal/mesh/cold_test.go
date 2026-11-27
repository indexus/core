package mesh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestDurableColdFoldTransfersDataReceiptsAndEmptyKeyspace(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, nodes := meshFixture(t, 2)
	nodes[1].VNode.ID = tree.ID{0, 1}
	records := []storage.Record{}
	for i, location := range []string{"0", "00", "000", tree.EncodeName(tree.ID{192})} {
		r := storage.Record{Collection: "0000000000000001", Location: location, ID: fmt.Sprintf("row-%d", i), Metrics: []float64{float64(i)}}
		if _, err := nodes[0].Write(ctx, "seed-"+r.ID, r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].Sync(ctx, []string{nodes[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	before, err := nodes[1].Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if err := nodes[1].Compact(ctx, ColdConfig{}); err != nil {
		t.Fatal(err)
	}
	status, err := nodes[1].Store.ColdStatus()
	if err != nil || status.Drain == nil || !status.Drain.Witnessed {
		t.Fatalf("retirement not witnessed: %+v %v", status, err)
	}
	target, err := nodes[0].Store.ColdStatus()
	if err != nil || target.Pin != nil {
		t.Fatalf("survivor still pinned: %+v %v", target, err)
	}
	snapshot, err := nodes[0].Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Current().Len() != 1 {
		t.Fatal("vnode was not removed")
	}
	rows, err := nodes[0].Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]storage.Record{}
	for _, zone := range rows {
		for _, r := range zone.Records {
			actual[r.Location+":"+r.ID] = r
		}
	}
	for _, r := range records {
		if !reflect.DeepEqual(actual[r.Location+":"+r.ID], r) {
			t.Fatal("payload lost during compaction")
		}
	}
	for _, r := range records {
		if _, _, err := nodes[1].Store.Apply("stale-"+r.ID, r); err == nil {
			t.Fatal("retired disk accepted a write")
		}
		mutation, applied, err := nodes[0].Store.Apply("seed-"+r.ID, r)
		if err != nil || applied || mutation.Seq == 0 {
			t.Fatalf("receipt lost: applied=%t error=%v", applied, err)
		}
	}
	if err := nodes[1].Store.MergeMembership(before.Membership); err != nil {
		t.Fatal(err)
	}
	if err := nodes[1].Join(ctx); err == nil {
		t.Fatal("retired vnode joined again")
	}
	// A previously empty key in the retired cell is now writable at the survivor.
	fresh := storage.Record{Collection: "0000000000000001", Location: "00000", ID: "fresh"}
	response, err := nodes[1].Write(ctx, "after-fold", fresh)
	if err != nil || response.AppliedBy != nodes[0].Self {
		t.Fatalf("retired ingress did not forward to survivor: %+v %v", response, err)
	}
	state, err := nodes[0].Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	replica, err := causal.NewMembership(state.Root, state.Genesis)
	if err != nil {
		t.Fatal(err)
	}
	if err := replica.Merge(state.Membership); err != nil || replica.Current().HasName(nodes[1].VNode.Name) {
		t.Fatalf("history failed to reconstruct: %v", err)
	}
}

// Both disks are reopened after every injected cut. A cached replica or a
// successful RPC return therefore cannot substitute for a durable permission.
func TestColdFoldRecoversEveryMessageCut(t *testing.T) {
	t.Parallel()
	for _, cut := range []string{"before:install-zone", "after:cold-pin", "after:install-zone", "after:stage", "after:install", "before:cold-witness", "after:cold-witness"} {
		t.Run(cut, func(t *testing.T) {
			t.Parallel() // each cut has its own fixture
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			net, nodes, reopen := coldRestartFixture(t)
			expected := map[string]storage.Record{}
			for i := 0; i < 256; i++ {
				r := storage.Record{Collection: "0000000000000001", Location: []string{"0", "00", "000"}[i%3], ID: fmt.Sprintf("row-%04d", i), Metrics: []float64{float64(i)}}
				if _, err := nodes[0].Write(ctx, "seed:"+r.ID, r); err != nil {
					t.Fatal(err)
				}
				expected[r.Location+":"+r.ID] = r
			}
			if err := nodes[1].Join(ctx); err != nil {
				t.Fatal(err)
			}
			if err := nodes[0].Sync(ctx, []string{nodes[1].Self.Host}); err != nil {
				t.Fatal(err)
			}
			dropped := false
			inject := func(phase string) func(Request) error {
				return func(req Request) error {
					if cut == phase+":"+req.Kind && !dropped {
						dropped = true
						// A process cut must escape bounded transport retries before reopen.
						return errors.Join(storage.ErrClosed, errors.New("injected cold cut"))
					}
					return nil
				}
			}
			net.setBefore(inject("before"))
			net.setAfter(inject("after"))
			if err := nodes[1].Compact(ctx, ColdConfig{}); err == nil || !dropped {
				t.Fatalf("cut not exercised: dropped=%t err=%v", dropped, err)
			}
			net.setBefore(nil)
			net.setAfter(nil)
			// At a stopped network boundary, all ACKed rows remain exact whenever a
			// complete read is available. Frozen reads must report unavailability.
			assertColdRows(t, ctx, nodes, expected, true)
			reopen()
			if err := nodes[1].Resume(ctx); err != nil {
				t.Fatal(err)
			}
			assertColdRows(t, ctx, nodes, expected, false)
			status, _ := nodes[1].Store.ColdStatus()
			pin, _ := nodes[0].Store.ColdStatus()
			if status.Drain == nil || !status.Drain.Witnessed || pin.Pin != nil {
				t.Fatalf("unfinished: %+v %+v", status, pin)
			}
			for _, r := range expected {
				_, applied, err := nodes[0].Store.Apply("seed:"+r.ID, r)
				if err != nil || applied {
					t.Fatalf("receipt lost: %v %t", err, applied)
				}
				if _, _, err := nodes[1].Store.Apply("stale:"+r.ID, r); err == nil {
					t.Fatal("retired source writable")
				}
			}
		})
	}
}

func coldRestartFixture(t *testing.T) (*network, []*Service, func()) {
	t.Helper()
	net := &network{nodes: map[string]*Service{}, blocked: map[string]bool{}}
	nodes := make([]*Service, 2)
	paths := make([]string, 2)
	for i := range nodes {
		name := fmt.Sprintf("cold-%d", i)
		paths[i] = filepath.Join(t.TempDir(), "data.db")
		store, err := storage.OpenWithOptions(paths[i], name, "cold", 1<<26, testStore)
		if err != nil {
			t.Fatal(err)
		}
		holder, _ := store.Endpoint(name)
		nodes[i] = &Service{Store: store, Self: holder, VNode: tree.VNodeDescriptor{Name: name, ID: tree.ID{byte(1 - i), 1}}, Peers: net}
		net.nodes[name] = nodes[i]
	}
	genesis := causal.Genesis{Cluster: "cold", Holder: nodes[0].Self}
	for _, node := range nodes {
		if err := node.Store.InitCausal(nodes[0].VNode, genesis); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, node := range nodes {
			node.Store.Close()
		}
	})
	return net, nodes, func() {
		for i, node := range nodes {
			old := node.Self
			if err := node.Store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := storage.OpenWithOptions(paths[i], node.VNode.Name, "cold", 1<<26, testStore)
			if err != nil {
				t.Fatal(err)
			}
			holder, _ := store.Endpoint(old.Host)
			if holder != old {
				t.Fatal("disk incarnation changed")
			}
			nodes[i] = &Service{Store: store, Self: holder, VNode: node.VNode, Peers: net}
			net.nodes[holder.Host] = nodes[i]
		}
	}
}
func assertColdRows(t *testing.T, ctx context.Context, nodes []*Service, expected map[string]storage.Record, allowUnavailable bool) {
	t.Helper()
	for _, node := range nodes {
		snapshots, err := node.Query(ctx)
		if allowUnavailable && (errors.Is(err, storage.ErrFrozen) || errors.Is(err, storage.ErrNotReady) || errors.Is(err, storage.ErrNotOwner)) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		actual := map[string]storage.Record{}
		for _, snap := range snapshots {
			for _, r := range snap.Records {
				key := r.Location + ":" + r.ID
				if _, ok := actual[key]; ok {
					t.Fatal("duplicate row")
				}
				actual[key] = r
			}
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("read lost or changed rows: got %d want %d", len(actual), len(expected))
		}
	}
}

func TestColdFoldHeatPreemptsAndLostReleaseResumes(t *testing.T) {
	t.Parallel()
	for _, heat := range []string{"source", "survivor", "lost-release-ack"} {
		t.Run(heat, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			net, nodes, reopen := coldRestartFixture(t)
			row := storage.Record{Collection: "0000000000000001", Location: "00", ID: "seed", Metrics: []float64{1}}
			if _, err := nodes[0].Write(ctx, "seed", row); err != nil {
				t.Fatal(err)
			}
			if err := nodes[1].Join(ctx); err != nil {
				t.Fatal(err)
			}
			if err := nodes[0].Sync(ctx, []string{nodes[1].Self.Host}); err != nil {
				t.Fatal(err)
			}
			heated := false
			released := false
			hot := storage.Record{Collection: row.Collection, Location: "000", ID: "hot", Metrics: []float64{2}}
			net.setAfter(func(req Request) error {
				if req.Kind == KindColdPin && !heated {
					heated = true
					if err := nodes[0].Store.BeginCausalMove(nodes[1].Self); !errors.Is(err, storage.ErrNotReady) {
						return fmt.Errorf("pinned survivor admitted MOVE: %v", err)
					}
					writer := nodes[1]
					if heat == "survivor" {
						writer = nodes[0]
						hot.Location = tree.EncodeName(tree.ID{1})
					}
					// Two writes exceed a budget of one before any piece starts.
					for i := 0; i < 2; i++ {
						if _, _, err := writer.Store.Apply(fmt.Sprint("hot", i), hot); err != nil {
							return err
						}
					}
				}
				if req.Kind == KindColdRelease && heat == "lost-release-ack" && !released {
					released = true
					return errors.New("lost release ACK")
				}
				return nil
			})
			err := nodes[1].Compact(ctx, ColdConfig{HeatBudget: 1})
			if !heated || err == nil {
				t.Fatalf("heat not exercised: %t %v", heated, err)
			}
			net.setAfter(nil)
			if heat == "lost-release-ack" {
				if !released {
					t.Fatal("release not exercised")
				}
				reopen()
				if err := nodes[1].Resume(ctx); !errors.Is(err, protocol.ErrColdPlanPreempted) {
					t.Fatalf("resume abort: %v", err)
				}
			} else if !errors.Is(err, protocol.ErrColdPlanPreempted) {
				t.Fatal(err)
			}
			source, _ := nodes[1].Store.ColdStatus()
			target, _ := nodes[0].Store.ColdStatus()
			if source.Drain != nil || target.Pin != nil {
				t.Fatalf("preemption left seals: %+v %+v", source, target)
			}
			snapshot, _ := nodes[1].Store.CausalRouting()
			if snapshot.Current().Len() != 2 {
				t.Fatal("hot vnode retired")
			}
			expected := map[string]storage.Record{row.Location + ":" + row.ID: row, hot.Location + ":" + hot.ID: hot}
			assertColdRows(t, ctx, nodes, expected, false)
			for _, node := range nodes {
				if err := node.Sync(ctx, []string{nodes[0].Self.Host, nodes[1].Self.Host}); err != nil {
					t.Fatal(err)
				}
			}
			if err := nodes[1].Rebalance(ctx); err != nil {
				t.Fatal(err)
			}
			if err := nodes[1].Compact(ctx, ColdConfig{}); err != nil {
				t.Fatalf("later quiet trial: %v", err)
			}
			assertColdRows(t, ctx, nodes, expected, false)
		})
	}
}

func TestLateColdMessagesDoNotDisturbNextSurvivorPin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nodes := meshFixture(t, 3)
	for _, node := range nodes[1:] {
		if err := node.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range nodes {
		if err := node.Sync(ctx, []string{nodes[0].Self.Host, nodes[1].Self.Host, nodes[2].Self.Host}); err != nil {
			t.Fatal(err)
		}
	}
	if err := nodes[1].Compact(ctx, ColdConfig{}); err != nil {
		t.Fatal(err)
	}
	status, _ := nodes[1].Store.ColdStatus()
	work := status.Drain
	if _, err := nodes[0].Store.PinColdSurvivor("next-plan", nodes[2].Self, storage.ColdOffer{}); err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].Store.WitnessColdRetirement(work.Operation, work.Source, *work.Retirement); err != nil {
		t.Fatalf("lost witness ACK is not replayable: %v", err)
	}
	if err := nodes[0].Store.ReleaseColdPin(work.Operation, work.Source); err != nil {
		t.Fatal(err)
	}
	pin, _ := nodes[0].Store.ColdStatus()
	if pin.Pin == nil || pin.Pin.Operation != "next-plan" {
		t.Fatal("delayed old message released next pin")
	}
}

func TestDurableColdFoldAllowsAnotherGrowthCycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, nodes := meshFixture(t, 3)
	expected := map[string]storage.Record{}
	for i := 0; i < 64; i++ {
		row := storage.Record{Collection: "0000000000000001", Location: []string{"0", "00", "000"}[i%3], ID: fmt.Sprintf("cycle-row-%d", i), Metrics: []float64{float64(i)}}
		if _, err := nodes[0].Write(ctx, "seed:"+row.ID, row); err != nil {
			t.Fatal(err)
		}
		expected[row.Location+":"+row.ID] = row
	}
	for i := 1; i < 3; i++ {
		if err := nodes[i].Join(ctx); err != nil {
			t.Fatal(err)
		}
		if err := nodes[0].Sync(ctx, []string{nodes[i].Self.Host}); err != nil {
			t.Fatal(err)
		}
		during := storage.Record{Collection: "0000000000000001", Location: "0000", ID: fmt.Sprintf("after-growth-%d", i), Metrics: []float64{100}}
		if _, err := nodes[i].Write(ctx, during.ID, during); err != nil {
			t.Fatal(err)
		}
		expected[during.Location+":"+during.ID] = during
		if err := nodes[i].Compact(ctx, ColdConfig{}); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := nodes[0].Store.CausalRouting()
		if snapshot.Current().Len() != 1 {
			t.Fatal("cycle did not compact")
		}
		assertColdRows(t, ctx, nodes[:i+1], expected, false)
	}
	for _, r := range expected {
		if len(r.ID) >= 9 && r.ID[:9] == "cycle-row" {
			_, applied, err := nodes[0].Store.Apply("seed:"+r.ID, r)
			if err != nil || applied {
				t.Fatal("receipt lost over two cycles")
			}
		}
	}
	t.Log("durable scale 1 -> 2 -> 1 -> 2 -> 1; fresh identities, insertions and exact reads")
}

func TestColdFoldSingleWriterAtTransferBoundaries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	net, nodes, _ := coldRestartFixture(t)
	expected := map[string]storage.Record{}
	var rows []storage.Record
	for i, path := range []string{"0", "00", "000", "00000"} {
		row := storage.Record{Collection: "0000000000000001", Location: path, ID: "same-id", Metrics: []float64{float64(i)}}
		rows = append(rows, row)
		expected[path+":"+row.ID] = row
		if i < 3 {
			if _, err := nodes[0].Write(ctx, "seed:"+path, row); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].Sync(ctx, []string{nodes[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	checks, zero := 0, 0
	probe := func(req Request) error {
		switch req.Kind {
		case "preload", "mirror", "stage-zone", "install-zone", "stage", "install", KindColdWitness:
		default:
			return nil
		}
		checks++
		for _, row := range rows {
			writers := 0
			for _, node := range nodes {
				_, applied, err := node.Store.Apply(fmt.Sprintf("probe:%d:%s", checks, row.Location), row)
				if err == nil {
					if applied {
						writers++
					}
					continue
				}
				if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) {
					return err
				}
			}
			if writers > 1 {
				return fmt.Errorf("%s: %d writers for %s", req.Kind, writers, row.Location)
			}
			if writers == 0 {
				zero++
			}
		}
		return nil
	}
	net.setBefore(probe)
	net.setAfter(probe)
	// COLD-2: the probes' writes stay within the heat budget and land while
	// pieces are moving, so the fold completes instead of sending data back.
	if err := nodes[1].Compact(ctx, ColdConfig{}); err != nil {
		t.Fatalf("fold under light writes: %v", err)
	}
	if checks < 8 || zero == 0 {
		t.Fatalf("insufficient boundaries: checks=%d frozen gaps=%d", checks, zero)
	}
	net.setBefore(nil)
	net.setAfter(nil)
	assertColdRows(t, ctx, nodes, expected, false)
	snapshot, err := nodes[1].Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Current().HasName(nodes[1].Self.VNode) {
		t.Fatal("source not retired")
	}
	t.Logf("%d stopped network boundaries, %d legitimate zero-writer probes, no duplicate writer", checks, zero)
}

func TestColdFoldUsesAcyclicSurvivorPreference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nodes := meshFixture(t, 4)
	for _, node := range nodes[1:] {
		if err := node.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var hosts []string
	for _, node := range nodes {
		hosts = append(hosts, node.Self.Host)
	}
	for _, node := range nodes {
		if err := node.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
	}
	// 161 and 241 are siblings, and either geometrically fits into the other.
	// Only 241 may begin closing; 161 stays available as its survivor.
	if err := nodes[2].Compact(ctx, ColdConfig{}); !errors.Is(err, protocol.ErrNoBetterConfiguration) {
		t.Fatalf("lower-ranked sibling started drain: %v", err)
	}
	status, _ := nodes[2].Store.ColdStatus()
	if status.Drain != nil {
		t.Fatal("preferred survivor sealed itself")
	}
	if err := nodes[3].Compact(ctx, ColdConfig{}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := nodes[2].Store.CausalRouting()
	if snapshot.Current().Len() != 3 || !snapshot.Current().HasName(nodes[2].VNode.Name) {
		t.Fatal("wrong sibling retired")
	}
}
