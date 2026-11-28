package mesh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type network struct {
	before  func(Request) error
	after   func(Request) error
	mu      sync.RWMutex
	nodes   map[string]*Service
	blocked map[string]bool
	hosts   []string
}

// Replication and asynchronous knowledge RPCs may still be in flight when a
// test changes a partition. Use the same lock as Exchange's lookup.
func (n *network) setBlocked(host string, blocked bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blocked[host] = blocked
}

func (n *network) setBefore(fn func(Request) error) { n.mu.Lock(); n.before = fn; n.mu.Unlock() }
func (n *network) setAfter(fn func(Request) error)  { n.mu.Lock(); n.after = fn; n.mu.Unlock() }
func (n *network) Exchange(ctx context.Context, host string, request Request) (Response, error) {
	n.mu.Lock()
	service, blocked := n.nodes[host], n.blocked[host]
	n.hosts = append(n.hosts, host)
	before, after := n.before, n.after
	n.mu.Unlock()
	if blocked || service == nil {
		return Response{}, fmt.Errorf("partition")
	}
	if before != nil {
		if err := before(request); err != nil {
			return Response{}, err
		}
	}
	response, err := service.Handle(ctx, request)
	if err == nil && after != nil {
		err = after(request)
	}
	return response, err
}
func meshFixture(t testing.TB, count int, endpoints ...string) (*network, []*Service) {
	t.Helper()
	net := &network{nodes: map[string]*Service{}, blocked: map[string]bool{}}
	var nodes []*Service
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("node-%d", i)
		store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), name, "test", 1<<26, testStore)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		host := name
		if len(endpoints) > 0 {
			host = endpoints[i]
		}
		holder, _ := store.Endpoint(host)
		s := &Service{Store: store, Self: holder, VNode: tree.VNodeDescriptor{Name: name, ID: tree.ID{byte(1 + i*80), 1}}, Peers: net}
		t.Cleanup(s.Close)
		net.nodes[host] = s
		nodes = append(nodes, s)
	}
	genesis := causal.Genesis{Cluster: "test", Holder: nodes[0].Self}
	for _, node := range nodes {
		if err := node.Store.InitCausal(nodes[0].VNode, genesis); err != nil {
			t.Fatal(err)
		}
	}
	return net, nodes
}

func TestConcurrentActivationPublicationUsesOneRecord(t *testing.T) {
	t.Parallel()
	_, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	view := tree.NewView(0)
	if err := view.Add(source.VNode.Name, source.VNode.ID); err != nil {
		t.Fatal(err)
	}
	if err := view.Add(target.VNode.Name, target.VNode.ID); err != nil {
		t.Fatal(err)
	}
	region, err := tree.Cell(view, target.VNode.Name)
	if err != nil {
		t.Fatal(err)
	}
	// Empty space installs a real right and publishes activation exactly once.
	var published atomic.Int32
	target.Activated = func() { published.Add(1) }
	if err := source.handoff(context.Background(), Request{Input: state.Permissions.Native[0].ID, Target: target.Self, TargetVNode: target.VNode, Region: region}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	failures := make(chan error, 32)
	var wait sync.WaitGroup
	for range 32 {
		wait.Go(func() {
			<-start
			failures <- target.publishActivation()
		})
	}
	close(start)
	wait.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if published.Load() != 1 {
		t.Fatalf("activation published %d times", published.Load())
	}
	replica, err := target.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if !replica.Current().HasName(target.VNode.Name) {
		t.Fatal("concurrent publication did not retain the active member")
	}
	_, _, membership, err := target.Store.Membership()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, record := range membership.Members {
		if record.VNode.Name == target.VNode.Name {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent publication retained %d member records", count)
	}
}

func TestWriterLearnsJoinAndRoutesNextWriteDirectly(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	ctx := context.Background()
	writer, err := NewWriter(ctx, net, []string{nodes[0].Self.Host})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	err = writer.mergeMembership(MembershipEnvelope{Root: nodes[0].VNode, Genesis: causal.Genesis{Cluster: "other", Holder: nodes[0].Self}})
	if !errors.Is(err, storage.ErrWrongCluster) {
		t.Fatalf("writer accepted knowledge from another cluster: %v", err)
	}
	root, genesis, delta, err := nodes[0].Store.Membership()
	if err != nil {
		t.Fatal(err)
	}
	knowledge := MembershipEnvelope{Root: root, Genesis: genesis, Delta: delta}
	poisoned := knowledge
	poisoned.Delta.Members = []causal.Member{{}}
	fresh := &Writer{}
	if err = fresh.mergeMembership(poisoned); err == nil || fresh.membership != nil {
		t.Fatalf("invalid first response poisoned writer: %v", err)
	}
	if err = fresh.mergeMembership(knowledge); err != nil {
		t.Fatalf("writer did not recover from rejected first response: %v", err)
	}
	if err = nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		for _, node := range nodes {
			if err = node.Sync(ctx, []string{nodes[0].Self.Host, nodes[1].Self.Host}); err != nil {
				t.Fatal(err)
			}
			if err = node.Resume(ctx); err != nil {
				t.Fatal(err)
			}
			if err = node.Rebalance(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	state, err := nodes[0].Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	replica, err := state.MembershipReplica()
	if err != nil {
		t.Fatal(err)
	}
	var record storage.Record
	for i := 1; i < 256; i++ {
		candidate := storage.Record{Collection: tree.EncodeName(tree.ID{byte(i), 1}), Location: tree.BASE64.Root(), ID: "first"}
		zone, e := tree.NewZoneKey(candidate.Collection, candidate.Location)
		if e != nil {
			t.Fatal(e)
		}
		if replica.Current().NameOf(replica.Current().Owner(zone.ID)) == nodes[1].Self.VNode {
			record = candidate
			break
		}
	}
	if record.Collection == "" {
		t.Fatal("could not find a zone owned by the joined vnode")
	}
	response, err := writer.Write(ctx, "first", record)
	if err != nil {
		t.Fatal(err)
	}
	if response.AppliedBy != nodes[1].Self {
		t.Fatalf("write applied by %v, want %v", response.AppliedBy, nodes[1].Self)
	}
	record.ID = "second"
	if _, err = writer.Write(ctx, "second", record); err != nil {
		t.Fatal(err)
	}
	// The exact-zone ACK hint routes the next write without merging topology.
	// A dedicated exchange can independently bring the global view up to date.
	if stats := writer.Stats(); stats.Forwarded != 1 || stats.Direct != 1 {
		t.Fatalf("writer did not learn exact-zone route: %+v", stats)
	}
	if err := writer.refreshView(ctx); err != nil {
		t.Fatal(err)
	}
	stats := writer.Stats()
	if stats.Forwarded != 1 || stats.Direct != 1 || stats.Active != 2 {
		t.Fatalf("writer did not converge after stale route: %+v", stats)
	}
	net.mu.Lock()
	net.hosts = nil
	net.mu.Unlock()
	record.ID = "third"
	if _, err = nodes[0].Write(ctx, "third", record); err != nil {
		t.Fatal(err)
	}
	net.mu.RLock()
	hosts := append([]string(nil), net.hosts...)
	net.mu.RUnlock()
	if len(hosts) != 1 || hosts[0] != nodes[1].Self.Host {
		t.Fatalf("ingress node did not route directly: %v", hosts)
	}
}

func TestWriterDoesNotHoldTopologyLockAcrossExchange(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 1)
	writer, err := NewWriter(context.Background(), net, []string{nodes[0].Self.Host})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	net.setBefore(func(request Request) error {
		if request.Kind == "write" {
			started <- struct{}{}
			<-release
		}
		return nil
	})
	errs := make(chan error, 2)
	for i := range 2 {
		go func() {
			record := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: fmt.Sprintf("concurrent-%d", i)}
			_, err := writer.Write(context.Background(), record.ID, record)
			errs <- err
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("writer serialized an exchange while holding its topology mutex")
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	stats := writer.Stats()
	if stats.Requests != 2 || stats.Direct != 2 {
		t.Fatalf("writer stats after concurrent calls: %+v", stats)
	}
}

func TestConcurrentJoinsStaleRoutingAndPartitionHeal(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 3)
	ctx := context.Background()
	records := make([]storage.Record, 80)
	for i := range records {
		records[i] = storage.Record{Collection: fmt.Sprintf("Collection%06d", i), Location: "aa", ID: "row", Metrics: []float64{float64(i)}}
		if _, err := nodes[0].Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprint(i), Record: &records[i]}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, node := range nodes[1:] {
		wg.Go(func() { failures <- node.Join(ctx) })
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Initial root remains a valid stale entry point. The request follows durable
	// successors and carries topology back along the same request path.
	for i := range records {
		records[i].Metrics[0]++
		if _, err := nodes[0].Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("updated-%d", i), Record: &records[i]}); err != nil {
			t.Fatal(err)
		}
	}
	net.setBlocked(nodes[2].Self.Host, true)
	if err := nodes[0].Sync(ctx, []string{nodes[2].Self.Host}); err == nil {
		t.Fatal("partition hidden")
	}
	net.setBlocked(nodes[2].Self.Host, false)
	for range 3 {
		for _, node := range nodes {
			if err := node.Sync(ctx, []string{nodes[0].Self.Host, nodes[1].Self.Host, nodes[2].Self.Host}); err != nil {
				t.Fatal(err)
			}
			if err := node.Rebalance(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	var digest causal.Digest
	total := 0
	for i, node := range nodes {
		state, err := node.Store.CausalState()
		if err != nil {
			t.Fatal(err)
		}
		replica, err := state.MembershipReplica()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			digest = replica.Digest()
		} else if replica.Digest() != digest {
			t.Fatal("topology divergence")
		}
		inventory, err := node.Store.Inventory(false)
		if err != nil {
			t.Fatal(err)
		}
		for _, snap := range inventory {
			if replica.Current().NameOf(replica.Current().Owner(snap.Zone.ID)) != node.Self.VNode {
				t.Fatal("converged authority differs from XOR")
			}
			for _, record := range snap.Records {
				if !record.Deleted {
					total++
				}
			}
		}
	}
	if total != len(records) {
		t.Fatalf("data conservation %d/%d", total, len(records))
	}
	// Every possible first byte still has exactly one installed current authority.
	for key := 0; key < 256; key++ {
		record := storage.Record{Collection: fmt.Sprintf("Probe%011d", key), Location: "aa", ID: "probe"}
		acks := 0
		for _, node := range nodes {
			_, _, err := node.Store.Apply(fmt.Sprintf("probe-%d", key), record)
			if err == nil {
				acks++
			} else if !errors.Is(err, storage.ErrNotOwner) {
				t.Fatal(err)
			}
		}
		if acks != 1 {
			t.Fatalf("%d writers for zone %d", acks, key)
		}
	}
}

func TestMoveHostKeepsXORAndFencesOldDisk(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 1)
	source := nodes[0]
	ctx := context.Background()
	writer, err := NewWriter(ctx, net, []string{source.Self.Host})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	record := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "one"}
	if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "put", Record: &record}); err != nil {
		t.Fatal(err)
	}
	state, _ := source.Store.CausalState()
	before, _ := state.MembershipReplica()
	targetStore, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "target.db"), source.Self.VNode, "test", 1<<26, testStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { targetStore.Close() })
	holder, _ := targetStore.Endpoint("new-host")
	target := &Service{Store: targetStore, Self: holder, VNode: source.VNode, Peers: net}
	t.Cleanup(target.Close)
	if err := targetStore.InitCausalReceiver(state.Root, state.Genesis); err != nil {
		t.Fatal(err)
	}
	net.mu.Lock()
	net.nodes[holder.Host] = target
	net.mu.Unlock()
	if err := target.ReceiveMove(ctx, source.Self.Host); err != nil {
		t.Fatal(err)
	}
	installedTarget, err := target.Store.Endpoint(target.Self.Host)
	if err != nil || installedTarget.Epoch != 1 {
		t.Fatalf("first physical endpoint not installed: %+v %v", installedTarget, err)
	}
	afterState, _ := target.Store.CausalState()
	after, _ := afterState.MembershipReplica()
	if before.Current().Digest != after.Current().Digest {
		t.Fatal("MOVE changed logical topology")
	}
	if _, _, err := source.Store.Apply("stale-direct", record); !errors.Is(err, storage.ErrNotOwner) {
		t.Fatalf("old disk ACK: %v", err)
	}
	if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "forwarded", Record: &record}); err != nil {
		t.Fatal(err)
	}
	record.ID = "writer-after-move"
	if response, err := writer.Write(ctx, record.ID, record); err != nil || response.AppliedBy != installedTarget {
		t.Fatalf("stale writer did not follow MOVE: holder=%v want=%v err=%v", response.AppliedBy, installedTarget, err)
	}
	record.ID = "writer-direct-after-move"
	if _, err := writer.Write(ctx, record.ID, record); err != nil {
		t.Fatal(err)
	}
	if stats := writer.Stats(); stats.Forwarded != 1 || stats.Direct != 1 {
		t.Fatalf("writer did not learn moved address: %+v", stats)
	}
	if err := source.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := target.ReceiveMove(ctx, source.Self.Host); err != nil {
		t.Fatalf("replayed MOVE: %v", err)
	}
	inventory, err := target.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 1 || len(inventory[0].Records) != 3 {
		t.Fatal("MOVE lost data")
	}
	returned, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Store.InitCausalReceiver(returned.Root, returned.Genesis); err != nil {
		t.Fatal(err)
	}
	if err := source.ReceiveMove(ctx, target.Self.Host); err != nil {
		t.Fatalf("return MOVE: %v", err)
	}
	returnedEndpoint, err := source.Store.Endpoint(source.Self.Host)
	if err != nil || returnedEndpoint.Epoch != 2 {
		t.Fatalf("physical return did not advance endpoint: %+v %v", returnedEndpoint, err)
	}
	if _, _, err := target.Store.Apply("retired-again", record); !errors.Is(err, storage.ErrNotOwner) {
		t.Fatalf("second old holder ACK: %v", err)
	}
	if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "back-on-original-disk", Record: &record}); err != nil {
		t.Fatal(err)
	}

}

func TestMirrorConcurrentWritesAndLostInstallACK(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	ctx := context.Background()
	// Put the target at this zone to guarantee that the concurrent mutation moves.
	record := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "original", Metrics: []float64{1}}
	zone, err := tree.NewZoneKey(record.Collection, record.Location)
	if err != nil {
		t.Fatal(err)
	}
	target.VNode.ID = zone.ID
	if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "put", Record: &record}); err != nil {
		t.Fatal(err)
	}
	var inserted, lost bool
	net.setAfter(func(request Request) error {
		if request.Kind == "preload" && !inserted {
			inserted = true
			deletion := record
			deletion.Deleted = true
			if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "delete", Record: &deletion}); err != nil {
				return err
			}
			future := record
			future.ID = "created-during-copy"
			if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "new", Record: &future}); err != nil {
				return err
			}
		}
		if (request.Kind == "install" || request.Kind == "install-zone") && !lost {
			lost = true
			// Model a stopped worker, not a transient ACK loss (retried locally).
			return fmt.Errorf("lost durable install ACK: %w", storage.ErrClosed)
		}
		return nil
	})
	if err := target.Join(ctx); err == nil {
		t.Fatal("lost ACK was not injected")
	}
	if !inserted || !lost {
		t.Fatal("copy and install windows not exercised")
	}
	if err := source.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "put", Record: &record}); err != nil {
		t.Fatal(err)
	}
	inventory, err := target.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, snapshot := range inventory {
		for _, row := range snapshot.Records {
			if !row.Deleted {
				live[row.ID] = true
			}
		}
	}
	if len(live) != 1 || !live["created-during-copy"] {
		t.Fatalf("mirror or replay lost state: %v", live)
	}
}

func TestDivergentMirrorKeepsSourceWritable(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	ctx := context.Background()
	record := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "row"}
	zone, _ := tree.NewZoneKey(record.Collection, record.Location)
	target.VNode.ID = zone.ID
	if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "initial", Record: &record}); err != nil {
		t.Fatal(err)
	}
	// Every copy round lands a backlog above the freeze floor that grows each
	// round: the mirror is diverging, not merely trailing by a bounded amount.
	writes, burst := 0, freezeBacklogFloor+64
	net.setAfter(func(request Request) error {
		isPreload := request.Kind == "preload" && request.Transfer != nil && len(request.Transfer.Snapshots) > 0
		if !isPreload && request.Kind != "mirror" {
			return nil
		}
		burst += freezeBacklogFloor
		for i := 0; i < burst; i++ {
			writes++
			row := record
			row.ID = fmt.Sprintf("row-%d", writes)
			row.Metrics = []float64{float64(writes)}
			if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("concurrent-%d", writes), Record: &row}); err != nil {
				return err
			}
		}
		return nil
	})
	if err := target.Join(ctx); !errors.Is(err, protocol.ErrRhoDiverged) {
		t.Fatalf("expected divergence refusal: %v", err)
	}
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	for _, transfer := range state.Transfers {
		if transfer.Phase >= causal.TransferMoving {
			t.Fatal("divergent mirror froze donor")
		}
	}
	if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "still-writable", Record: &record}); err != nil {
		t.Fatal(err)
	}
	net.setAfter(nil)
	if err := target.Join(ctx); err != nil {
		t.Fatalf("convergent retry failed: %v", err)
	}
}

// Aggregate mirror progress, not one continuously-written zone, decides
// whether freeze is safe. Here total lag grows 257 -> 300 then shrinks to 258
// (below half the region) while the first 257 zones retain lag one.
func TestShrinkingAggregateMirrorCanFreezeWithStablePerZoneLag(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	ctx := context.Background()
	const zones = 520
	for i := range zones {
		record := storage.Record{Collection: fmt.Sprintf("Collection-%05d", i), Location: "aa", ID: "initial"}
		if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("initial-%04d", i), Record: &record}); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := tree.NewZoneKey("Collection-00000", "aa")
	target.VNode.ID = first.ID
	bursts := []int{257, 300, 258}
	round := 0
	net.setAfter(func(request Request) error {
		if request.Kind != "preload" || round >= len(bursts) {
			return nil
		}
		n := bursts[round]
		for i := range n {
			record := storage.Record{
				Collection: fmt.Sprintf("Collection-%05d", i),
				Location:   "aa",
				ID:         fmt.Sprintf("round-%d", round),
			}
			if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("round-%d-%04d", round, i), Record: &record}); err != nil {
				return err
			}
		}
		round++
		return nil
	})
	if err := target.Join(ctx); err != nil {
		t.Fatalf("shrinking aggregate backlog refused: %v", err)
	}
	if round < len(bursts) {
		t.Fatalf("fixture exercised only %d mirror rounds", round)
	}
}

func TestShadowReceiverResumesDroppedInstallWithoutDonorWorker(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dropped := false
	net.setBefore(func(request Request) error {
		if request.Kind == "install" && !dropped {
			dropped = true
			return fmt.Errorf("worker stopped before regional install: %w", storage.ErrClosed)
		}
		return nil
	})
	if err := nodes[1].Join(ctx); err == nil {
		t.Fatal("fault not exercised")
	}
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatalf("receiver cannot resume: %v", err)
	}
}

func TestCompetingJoinFinishesPersistedTokenReservation(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 3)
	source, first, second := nodes[0], nodes[1], nodes[2]
	ctx := context.Background()
	// Stop J's coordinator after reserving D's token but before receiver preload.
	blocked := true
	net.setBefore(func(request Request) error {
		if request.Kind == "preload" && blocked {
			return fmt.Errorf("coordinator disconnected")
		}
		return nil
	})
	if err := first.Join(ctx); err == nil {
		t.Fatal("failure not injected")
	}
	blocked = false
	// K must help finish the overlapping durable reservation without relying on
	// D or J having a background worker. Then it rebases to the successor token.
	if err := second.Join(ctx); err != nil {
		t.Fatalf("orphan reservation blocks competing join: %v", err)
	}
	if err := first.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := source.Sync(ctx, []string{first.Self.Host, second.Self.Host}); err != nil {
		t.Fatal(err)
	}
}

// ACT-1: a joiner with two donors activates as soon as the first region is
// installed. While the second donor is still being acquired, writes for that
// region reaching the joiner are forwarded to the donor, which keeps serving;
// writes for the installed region are applied on the joiner. Join then finishes
// the acquisition and the cell converges to one writer per zone.
func TestJoinActivatesOnFirstInstallAndForwardsTheRest(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	// node-2 (ID 0b1010_0001) takes every ID with a leading 1: 10... comes from
	// node-0 and 11... from node-1. A zone id starts with its location bits:
	// BASE64 'W' encodes 0b10_0000 and 'm' 0b11_0000.
	fromRoot := storage.Record{Collection: "Probe00000000000", Location: "W", ID: "row", Metrics: []float64{1}}
	fromFirst := storage.Record{Collection: "Probe00000000000", Location: "m", ID: "row", Metrics: []float64{1}}
	for _, record := range []storage.Record{fromRoot, fromFirst} {
		id, err := tree.ZoneID(record.Collection, record.Location)
		if err != nil || id[0]>>7 != 1 {
			t.Fatalf("probe %q outside the joiner's cell: %08b %v", record.Location, id[0], err)
		}
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	releaseDonor := func() { released.Do(func() { close(release) }) }
	defer releaseDonor()
	net.setBefore(func(request Request) error {
		if request.Kind == "preload" && request.Transfer != nil && request.Transfer.Cession.Source.Owner == nodes[1].Self.Owner {
			once.Do(func() { close(reached) })
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	joined := make(chan error, 1)
	go func() { joined <- nodes[2].Join(ctx) }()
	select {
	case <-reached:
	case err := <-joined:
		t.Fatalf("join returned before the second donor copied: %v", err)
	}
	// The first donor's region is installed; the joiner must be active now even
	// though the second donor is still copying its part of the cell.
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := nodes[2].Store.CausalState()
		if err != nil {
			t.Fatal(err)
		}
		replica, err := state.MembershipReplica()
		if err != nil {
			t.Fatal(err)
		}
		if replica.Current().HasName(nodes[2].VNode.Name) {
			break
		}
		if time.Now().After(deadline) {
			releaseDonor()
			t.Fatal("joiner not active after its first install")
		}
		time.Sleep(5 * time.Millisecond)
	}
	local, err := nodes[2].Handle(ctx, Request{Kind: "write", RequestID: "local", Record: &fromRoot})
	if err != nil || !local.AppliedLocally || local.AppliedBy != nodes[2].Self {
		t.Fatalf("installed region not applied on the joiner: %+v %v", local, err)
	}
	forwarded, err := nodes[2].Handle(ctx, Request{Kind: "write", RequestID: "forwarded", Record: &fromFirst})
	if err != nil || forwarded.AppliedLocally || forwarded.AppliedBy != nodes[1].Self {
		t.Fatalf("pending region not forwarded to its holder: %+v %v", forwarded, err)
	}
	releaseDonor()
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	net.setBefore(nil)
	for range 2 {
		for _, node := range nodes {
			if err := node.Sync(ctx, []string{nodes[0].Self.Host, nodes[1].Self.Host, nodes[2].Self.Host}); err != nil {
				t.Fatal(err)
			}
			if err := node.Rebalance(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Both rows now live on the joiner, each once.
	inventory, err := nodes[2].Store.Inventory(false)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, snap := range inventory {
		for _, record := range snap.Records {
			if !record.Deleted {
				seen[record.Location]++
			}
		}
	}
	if len(seen) != 2 || seen[fromRoot.Location] != 1 || seen[fromFirst.Location] != 1 {
		t.Fatalf("rows on the joiner after acquisition: %v", seen)
	}
	for _, key := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" {
		record := storage.Record{Collection: "Probe00000000000", Location: string(key), ID: "probe"}
		acks := 0
		for _, node := range nodes {
			if _, _, err := node.Store.Apply(fmt.Sprintf("probe-%c", key), record); err == nil {
				acks++
			} else if !errors.Is(err, storage.ErrNotOwner) {
				t.Fatal(err)
			}
		}
		if acks != 1 {
			t.Fatalf("%d writers for zone %c", acks, key)
		}
	}
}

func TestZONE1_WriteAckedByOneSideDuringMigration(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	nodes[0].FlipUnitZones = 1
	ctx := context.Background()
	var inCell []storage.Record
	view := tree.NewView(0)
	_ = view.Add(nodes[0].VNode.Name, nodes[0].VNode.ID)
	_ = view.Add(nodes[1].VNode.Name, nodes[1].VNode.ID)
	for _, loc := range []string{"W", "X", "m", "n", "aa", "ab"} {
		record := storage.Record{Collection: "Probe00000000000", Location: loc, ID: "row", Metrics: []float64{1}}
		id, err := tree.ZoneID(record.Collection, record.Location)
		if err != nil {
			t.Fatal(err)
		}
		if view.Owner(id) == nodes[1].VNode.ID {
			inCell = append(inCell, record)
		}
	}
	if len(inCell) < 2 {
		t.Fatalf("need two zones in the joiner cell, got %d", len(inCell))
	}
	first, second := inCell[0], inCell[1]
	if _, err := nodes[0].Write(ctx, "seed-a", first); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes[0].Write(ctx, "seed-b", second); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var flipped tree.ZoneKey
	var once sync.Once
	net.setAfter(func(request Request) error {
		if request.Kind == "install-zone" && len(request.ZoneTransfers) > 0 {
			once.Do(func() {
				flipped = request.ZoneTransfers[0].Zone
				close(reached)
			})
			<-release
		}
		return nil
	})
	joined := make(chan error, 1)
	go func() { joined <- nodes[1].Join(ctx) }()
	select {
	case <-reached:
	case err := <-joined:
		t.Fatalf("join returned before the first install-zone: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := nodes[1].Store.CausalState()
		if err != nil {
			t.Fatal(err)
		}
		replica, err := state.MembershipReplica()
		if err != nil {
			t.Fatal(err)
		}
		if replica.Current().HasName(nodes[1].VNode.Name) {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("joiner not active after its first install-zone")
		}
		time.Sleep(5 * time.Millisecond)
	}
	moved := storage.Record{Collection: flipped.Collection, Location: flipped.Location, ID: "row", Metrics: []float64{1}}
	local, err := nodes[1].Handle(ctx, Request{Kind: "write", RequestID: "local", Record: &moved})
	if err != nil || !local.AppliedLocally || local.AppliedBy != nodes[1].Self {
		close(release)
		t.Fatalf("transferred zone not applied on the joiner: %+v %v", local, err)
	}
	other := first
	if other.Location == flipped.Location {
		other = second
	}
	forwarded, err := nodes[1].Handle(ctx, Request{Kind: "write", RequestID: "forwarded", Record: &other})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if forwarded.AppliedLocally || forwarded.AppliedBy != nodes[0].Self {
		close(release)
		t.Fatal("both zones flipped at once")
	}
	acks := 0
	for _, node := range nodes {
		if _, _, err := node.Store.Apply("exclusive-"+flipped.Location, moved); err == nil {
			acks++
		} else if !errors.Is(err, storage.ErrNotOwner) && !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) {
			close(release)
			t.Fatal(err)
		}
	}
	if acks != 1 {
		close(release)
		t.Fatalf("%d writers for transferred zone", acks)
	}
	close(release)
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
}
