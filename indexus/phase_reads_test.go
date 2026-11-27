package indexus

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/testkit"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/wire"
)

var errReadPhasePartition = errors.New("injected phase partition")

type readPhaseTransport struct {
	nodes         map[string]*Node
	before, after func(mesh.Request) error
	blocked       atomic.Bool
}

func (n *readPhaseTransport) Exchange(ctx context.Context, host string, r mesh.Request) (mesh.Response, error) {
	if n.blocked.Load() {
		return mesh.Response{}, errReadPhasePartition
	}
	if n.before != nil {
		if err := n.before(r); err != nil {
			return mesh.Response{}, err
		}
	}
	node := n.nodes[host]
	if node == nil {
		return mesh.Response{}, errReadPhasePartition
	}
	response, err := node.MeshData(ctx, r)
	if err == nil && n.after != nil {
		err = n.after(r)
	}
	return response, err
}

type readPhaseFixture struct {
	net      *readPhaseTransport
	nodes    []*Node
	stores   []*storage.Store
	services []*mesh.Service
}

func newReadPhaseFixture(t *testing.T, descriptors []VNodeDescriptor, shadow, logical bool) *readPhaseFixture {
	t.Helper()
	net := &readPhaseTransport{nodes: map[string]*Node{}}
	var nodes []*Node
	var stores []*storage.Store
	var services []*mesh.Service
	var holders []causal.Endpoint
	for i, descriptor := range descriptors {
		settings, err := NewSettings(descriptor.Name, 9000+i, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		node, err := NewNode(settings, testNewContact, nil)
		if err != nil {
			t.Fatal(err)
		}
		store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), descriptor.Name, "read-phases", 1<<27, storage.Options{NoSync: logical, BatchWrites: true, BatchDelay: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		holder, _ := store.Endpoint(fmt.Sprintf("http://127.0.0.1:%d", 9000+i))
		nodes = append(nodes, node)
		stores = append(stores, store)
		holders = append(holders, holder)
		net.nodes[holder.Host] = node
		services = append(services, &mesh.Service{Store: store, Self: holder, VNode: descriptor, Peers: net})
	}
	genesis := causal.Genesis{Cluster: "read-phases", Holder: holders[0]}
	for i, store := range stores {
		var err error
		if shadow && i == 1 {
			err = store.InitCausalReceiver(descriptors[0], genesis)
		} else {
			err = store.InitCausal(descriptors[0], genesis)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := nodes[i].EnableCausalCluster(services[i]); err != nil {
			t.Fatal(err)
		}
	}

	return &readPhaseFixture{net, nodes, stores, services}
}

// Reads run at every network boundary while the handoff is paused there.
// Probes hit disks directly: forwarding ACKs from several ingress nodes must
// never be mistaken for several authoritative writers.
func TestReadsAndSingleWriterThroughHandoffAndLostMessages(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"none", "before-install", "lost-install-ack", "concurrent-joins", "move-host"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			collection := "0000000000000001"
			root := VNodeDescriptor{ID: ID{255}}
			root.Name = BASE64.Encode(root.ID.Bytes())
			zone, _ := NewZoneKey(collection, "0")
			target := VNodeDescriptor{ID: zone.ID}
			target.Name = BASE64.Encode(target.ID.Bytes())
			descriptors := []VNodeDescriptor{root, target}
			if fault == "move-host" {
				descriptors = []VNodeDescriptor{root, root}
			}
			if fault == "concurrent-joins" {
				intermediate := VNodeDescriptor{ID: ID{128}}
				intermediate.Name = BASE64.Encode(intermediate.ID.Bytes())
				descriptors = []VNodeDescriptor{root, intermediate, target}
			}
			fixture := newReadPhaseFixture(t, descriptors, fault == "move-host", false)
			net, nodes, stores, services := fixture.net, fixture.nodes, fixture.stores, fixture.services
			records := []storage.Record{{Collection: collection, Location: "0", ID: "same-row", Metrics: []float64{1}}, {Collection: collection, Location: "00", ID: "same-row", Metrics: []float64{2}}, {Collection: collection, Location: "000", ID: "same-row", Metrics: []float64{3}}}
			for _, row := range records {
				if _, err := nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "seed:" + row.Location, Record: &row}); err != nil {
					t.Fatal(err)
				}
			}
			if err := nodes[0].Update(); err != nil {
				t.Fatal(err)
			}
			var probeMu sync.Mutex
			checks, exact, unavailable, zeroWriters := 0, 0, 0, 0
			var changed, dropped atomic.Bool
			isCut := func(kind string) bool {
				switch kind {
				case "preload", "mirror", "stage-zone", "install-zone", "stage", "install":
					return true
				}
				return false
			}
			expectedCount := func(prefix string) int {
				count := 0
				for _, row := range records {
					if !row.Deleted && (prefix == "@" || len(row.Location) >= len(prefix) && row.Location[:len(prefix)] == prefix) {
						count++
					}
				}
				return count
			}
			expectedMetric := func(prefix string) float64 {
				sum := 0.0
				for _, row := range records {
					if !row.Deleted && (prefix == "@" || len(row.Location) >= len(prefix) && row.Location[:len(prefix)] == prefix) {
						sum += row.Metrics[0]
					}
				}
				return sum
			}
			probe := func(label string) error {
				probeMu.Lock()
				defer probeMu.Unlock()
				checks++
				for _, row := range records {
					type answer struct {
						applied bool
						err     error
					}
					answers := make(chan answer, len(stores))
					var wait sync.WaitGroup
					for disk, store := range stores {
						wait.Add(1)
						go func(disk int, store *storage.Store) {
							defer wait.Done()
							requestID := fmt.Sprintf("probe:%d:%d:%s:%s", checks, disk, row.Location, row.ID)
							// With concurrent handoffs, authority may move between two calls. A
							// common operation ID distinguishes a legitimate replay from two fresh
							// applications without assuming a simultaneous distributed snapshot.
							if fault == "concurrent-joins" {
								requestID = fmt.Sprintf("probe:%d:%s:%s", checks, row.Location, row.ID)
							}
							_, applied, err := store.Apply(requestID, row)
							answers <- answer{applied, err}
						}(disk, store)
					}
					wait.Wait()
					close(answers)
					writers := 0
					for a := range answers {
						if a.err == nil && a.applied {
							writers++
						} else if a.err == nil && fault == "concurrent-joins" {
							continue
						} else if !errors.Is(a.err, storage.ErrFrozen) && !errors.Is(a.err, storage.ErrNotReady) && !errors.Is(a.err, storage.ErrNotOwner) {
							return fmt.Errorf("%s write probe: %v", label, a.err)
						}
					}
					if writers > 1 {
						return fmt.Errorf("%s: %d authoritative writers for %s", label, writers, row.Location)
					}
					if writers == 0 {
						zeroWriters++
					}
				}
				for _, service := range services {
					inventory, err := service.Query(ctx)
					if err != nil {
						if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) && !errors.Is(err, errReadPhasePartition) {
							return fmt.Errorf("%s data snapshot: %w", label, err)
						}
						unavailable++
						continue
					}
					actual := map[Key]map[string]storage.Record{}
					for _, snapshot := range inventory {
						key := Key{Collection: snapshot.Zone.Collection, Location: snapshot.Zone.Location}
						if actual[key] == nil {
							actual[key] = map[string]storage.Record{}
						}
						for _, row := range snapshot.Records {
							if _, duplicate := actual[key][row.ID]; duplicate {
								return fmt.Errorf("%s duplicate authoritative row %s/%s", label, key.Location, row.ID)
							}
							actual[key][row.ID] = row
						}
					}
					expected := map[Key]map[string]storage.Record{}
					for _, row := range records {
						key := Key{Collection: row.Collection, Location: row.Location}
						if expected[key] == nil {
							expected[key] = map[string]storage.Record{}
						}
						expected[key][row.ID] = row
					}
					if !reflect.DeepEqual(expected, actual) {
						return fmt.Errorf("%s incomplete or changed data snapshot from %+v: got=%v expected=%v", label, service.Self, actual, expected)
					}
					exact++
				}
				for _, node := range nodes {
					for _, prefix := range []string{"@", "0", "00", "000"} {
						_, set, err := node.Get(collection, prefix, true, nil, true)
						if err != nil {
							if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) && !errors.Is(err, errReadPhasePartition) {
								return fmt.Errorf("%s refreshed read: %w", label, err)
							}
							unavailable++
							continue
						}
						if set == nil || set.Count() != expectedCount(prefix) || set.Abelian().Metric(0) != expectedMetric(prefix) {
							return fmt.Errorf("%s: false exact read at %s from %s: got=%v want=%d", label, prefix, node.Name(), set, expectedCount(prefix))
						}
						exact++
					}
				}
				return nil
			}
			if fault == "move-host" {
				answers := 0
				for _, node := range nodes {
					values, err := node.GetAggregates(collection, []string{"@"})
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := values["@"]; ok {
						answers++
					}
				}
				if answers != 1 {
					t.Fatalf("shadow disk served a second aggregate for the same vnode: %d responders", answers)
				}
			}
			if err := probe("initial"); err != nil {
				t.Fatal(err)
			}
			net.before = func(r mesh.Request) error {
				if !isCut(r.Kind) {
					return nil
				}
				if err := probe("before:" + r.Kind); err != nil {
					return err
				}
				if fault == "before-install" && r.Kind == "install-zone" && dropped.CompareAndSwap(false, true) {
					// Stop this worker; a transient message loss is now repaired locally.
					return errors.Join(storage.ErrClosed, errReadPhasePartition)
				}
				return nil
			}
			net.after = func(r mesh.Request) error {
				if !isCut(r.Kind) {
					return nil
				}
				if r.Kind == "preload" && changed.CompareAndSwap(false, true) {
					probeMu.Lock()
					added := storage.Record{Collection: collection, Location: "0", ID: "during-copy", Metrics: []float64{4}}
					_, err := nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "during-copy", Record: &added})
					if err == nil {
						records = append(records, added)
						records[2].Deleted = true
						removed := records[2]
						_, err = nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "during-copy-delete", Record: &removed})
					}
					probeMu.Unlock()
					if err != nil {
						return err
					}
				}
				if err := probe("after:" + r.Kind); err != nil {
					return err
				}
				if fault == "lost-install-ack" && r.Kind == "install-zone" && dropped.CompareAndSwap(false, true) {
					// Stop this worker; a transient message loss is now repaired locally.
					return errors.Join(storage.ErrClosed, errReadPhasePartition)
				}
				return nil
			}
			var err error
			if fault == "concurrent-joins" {
				failures := make(chan error, 2)
				var joining sync.WaitGroup
				for _, service := range services[1:] {
					joining.Add(1)
					go func(service *mesh.Service) { defer joining.Done(); failures <- service.Join(ctx) }(service)
				}
				joining.Wait()
				close(failures)
				for failure := range failures {
					if failure != nil {
						err = errors.Join(err, failure)
					}
				}
			} else if fault == "move-host" {
				err = services[1].ReceiveMove(ctx, services[0].Self.Host)
			} else {
				err = services[1].Join(ctx)
			}
			if fault == "before-install" || fault == "lost-install-ack" {
				if err == nil || !dropped.Load() {
					t.Fatal("fault not exercised")
				}
				if err := probe("interrupted"); err != nil {
					t.Fatal(err)
				}
				if err := services[0].Resume(ctx); err != nil {
					t.Fatal(err)
				}
				if err := services[1].Join(ctx); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !changed.Load() {
				t.Fatal("copy-time insert/delete not exercised")
			}
			// A new ACK through the retired ingress must become visible there too;
			// reading the unchanged pre-handoff value would mask a stranded cache.
			addedAfter := storage.Record{Collection: collection, Location: "0", ID: "after-handoff", Metrics: []float64{8}}
			if _, err := nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "after-handoff", Record: &addedAfter}); err != nil {
				t.Fatal(err)
			}
			records = append(records, addedAfter)
			if err := probe("completed"); err != nil {
				t.Fatal(err)
			}
			// Partition after the handoff: a stale remote route cannot turn missing
			// transport data into a successful empty refreshed read.
			net.blocked.Store(true)
			if err := probe("partitioned"); err != nil {
				t.Fatal(err)
			}
			net.blocked.Store(false)
			deadline := time.Now().Add(8 * time.Second)
			for {
				for _, node := range nodes {
					if err := node.Update(); err != nil {
						t.Fatal(err)
					}
				}
				converged := true
				for _, node := range nodes {
					for _, prefix := range []string{"@", "0", "00", "000"} {
						_, set, err := node.Get(collection, prefix, true, nil, false)
						if err != nil || set == nil || set.Count() != expectedCount(prefix) || set.Abelian().Metric(0) != expectedMetric(prefix) {
							converged = false
						}
					}
				}
				if converged {
					break
				}
				if time.Now().After(deadline) {
					for _, node := range nodes {
						state, _ := node.durableStore().CausalState()
						t.Logf("convergence node=%s rights=%+v transfers=%+v", node.Name(), state.Permissions.Native, state.Transfers)
						t.Logf("summary node=%s stats=%+v boundaries=%+v", node.Name(), node.mesh.summary.Stats(), node.mesh.summary.Export(collection, nil, ""))
						for _, prefix := range []string{"@", "0", "00", "000"} {
							_, got, e := node.Get(collection, prefix, true, nil, false)
							count := -1
							if got != nil {
								count = got.Count()
							}
							t.Logf("ordinary node=%s prefix=%s count=%d want=%d err=%v", node.Name(), prefix, count, expectedCount(prefix), e)
						}
					}
					t.Fatal("ordinary reads failed to converge after healing")
				}
				time.Sleep(50 * time.Millisecond)
			}
			for _, prefix := range []string{"@", "0", "00", "000"} {
				answers := 0
				for _, node := range nodes {
					values, err := node.GetAggregates(collection, []string{prefix})
					if err != nil {
						t.Fatal(err)
					}
					if value := values[prefix]; value != nil {
						answers++
						if value.Count() != expectedCount(prefix) || value.Metric(0) != expectedMetric(prefix) {
							t.Fatalf("incorrect aggregate %s", prefix)
						}
					}
				}
				if answers != 1 {
					t.Fatalf("%s: %d physical aggregate responders", prefix, answers)
				}
			}
			if fault == "move-host" {
				contact, set, err := nodes[0].Get(collection, "@", false, nil, false)
				if err != nil || set != nil || contact == nil || contact.Port() != 9001 {
					t.Fatalf("retired endpoint did not redirect to current physical host: %v %v %v", contact, set, err)
				}
				block, err := nodes[0].GetSetsBlockContext(context.Background(), collection, []string{"@"}, false, nil, false, nil, wire.SetsBlockF32, nil)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := wire.DecodeSetsBlock(block)
				if err != nil || len(decoded.Redirects) != 1 || decoded.Redirects[0].Port != 9001 || len(decoded.Sections) != 0 {
					t.Fatalf("physical host redirect lost in /sets block: %+v %v", decoded, err)
				}
			}
			if err := probe("healed"); err != nil {
				t.Fatal(err)
			}
			t.Logf("cuts=%d exact_reads=%d explicit_unavailable=%d zero_writer_observations=%d; ordinary reads converged", checks, exact, unavailable, zeroWriters)
		})
	}
}

// This run overlaps actual logical changes with readers, rather than pausing
// writes at a protocol cut. NoSync keeps this a concurrency stress test; the
// separate durable-cut suite exercises physical commits and crash reopening.
func TestContinuousReadsKeepEarlierACKsDuringJoinsAndPartition(t *testing.T) {
	t.Parallel()
	testContinuousReadsKeepEarlierACKsDuringJoinsAndPartition(t)
}

func testContinuousReadsKeepEarlierACKsDuringJoinsAndPartition(t *testing.T) {
	t.Helper()
	// 45 s holds a normal run; the race detector slows it several times.
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(45*time.Second))
	defer cancel()
	descriptors := []VNodeDescriptor{{ID: ID{255}}, {ID: ID{128}}, {ID: ID{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}}}
	for i := range descriptors {
		descriptors[i].Name = BASE64.Encode(descriptors[i].ID.Bytes())
	}
	f := newReadPhaseFixture(t, descriptors, false, true)
	const collection = "0000000000000001"
	const volume = 2000
	record := func(i int) storage.Record {
		location := "0"
		if i%2 == 1 {
			location = "00"
		}
		return storage.Record{Collection: collection, Location: location, ID: fmt.Sprintf("row-%d", i), Metrics: []float64{float64(i)}}
	}
	seed := record(0)
	if _, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: seed.ID, Record: &seed}); err != nil {
		t.Fatal(err)
	}
	var ack, reads, unavailable, sweeps atomic.Int64
	ack.Store(1)
	failures := make(chan error, 8)
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
		cancel()
	}
	transient := func(err error) bool {
		return errors.Is(err, storage.ErrFrozen) || errors.Is(err, storage.ErrNotReady) || errors.Is(err, storage.ErrNotOwner) || errors.Is(err, errReadPhasePartition)
	}
	readCtx, stopReads := context.WithCancel(ctx)
	defer stopReads()
	var reading sync.WaitGroup
	for _, node := range f.nodes {
		reading.Add(1)
		go func(node *Node) {
			defer reading.Done()
			for readCtx.Err() == nil {
				before := ack.Load()
				_, set, err := node.Get(collection, "@", true, nil, true)
				after := ack.Load()
				if err != nil {
					if readCtx.Err() != nil {
						return
					}
					if !transient(err) {
						fail(fmt.Errorf("continuous read: %w", err))
						return
					}
					unavailable.Add(1)
				} else {
					if set == nil || int64(set.Count()) < before || int64(set.Count()) > after+1 {
						count := -1
						if set != nil {
							count = set.Count()
						}
						fail(fmt.Errorf("read lost an earlier ACK or invented a row: before=%d after=%d count=%d", before, after, count))
						return
					}
					sum := set.Abelian().Metric(0)
					if sum < float64(before*(before-1)/2) || sum > float64((after+1)*after/2) {
						fail(fmt.Errorf("read metric outside acknowledged bounds: before=%d after=%d sum=%v", before, after, sum))
						return
					}
					reads.Add(1)
				}
				select {
				case <-readCtx.Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}(node)
	}
	// One common operation per sweep remains unique even if the holder moves
	// between two disk calls; copying its receipt makes the second a replay.
	reading.Add(1)
	go func() {
		defer reading.Done()
		for readCtx.Err() == nil {
			sequence := sweeps.Add(1)
			type result struct {
				applied bool
				err     error
			}
			results := make(chan result, len(f.stores))
			var applying sync.WaitGroup
			for _, store := range f.stores {
				applying.Add(1)
				go func(store *storage.Store) {
					defer applying.Done()
					_, applied, err := store.Apply(fmt.Sprintf("live-probe:%d", sequence), seed)
					results <- result{applied, err}
				}(store)
			}
			applying.Wait()
			close(results)
			fresh := 0
			for r := range results {
				if r.err == nil {
					if r.applied {
						fresh++
					}
				} else if !transient(r.err) {
					fail(r.err)
					return
				}
			}
			if fresh > 1 {
				fail(fmt.Errorf("%d fresh writers for the same operation during a handoff", fresh))
				return
			}
			select {
			case <-readCtx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	var joining sync.WaitGroup
	join := func(index int) {
		joining.Add(1)
		go func() {
			defer joining.Done()
			if err := f.services[index].Join(ctx); err != nil && ctx.Err() == nil {
				fail(err)
			}
		}()
	}
	for i := 1; i < volume && ctx.Err() == nil; i++ {
		row := record(i)
		for ctx.Err() == nil {
			_, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: row.ID, Record: &row})
			if err == nil {
				ack.Store(int64(i + 1))
				break
			}
			if !transient(err) {
				fail(err)
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if i == 150 {
			join(1)
		}
		if i == 500 {
			join(2)
		}
		if i == 750 {
			// Hold the partition until a foreground probe actually observes it.
			// A wall-clock-only cut can heal before any reader is scheduled under
			// -race, leaving the fail-closed guarantee entirely unexercised.
			probeErr := func() error {
				f.net.blocked.Store(true)
				defer f.net.blocked.Store(false)
				observed := false
				for _, node := range f.nodes {
					_, set, err := node.GetSetContext(ctx, collection, "@", true, nil, true)
					if err != nil {
						if !transient(err) {
							return err
						}
						unavailable.Add(1)
						observed = true
						break
					}
					if set == nil || int64(set.Count()) != ack.Load() {
						return fmt.Errorf("partition probe returned an incomplete successful read after %d ACKs", ack.Load())
					}
				}
				if !observed {
					return fmt.Errorf("partition did not exercise an unavailable remote read")
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(25 * time.Millisecond):
					return nil
				}
			}()
			if probeErr != nil {
				fail(probeErr)
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	joining.Wait()
	stopReads()
	reading.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	if ack.Load() != volume || reads.Load() == 0 || sweeps.Load() == 0 || unavailable.Load() == 0 {
		t.Fatalf("insufficient exercise: ack=%d reads=%d unavailable=%d sweeps=%d", ack.Load(), reads.Load(), unavailable.Load(), sweeps.Load())
	}
	f.net.blocked.Store(false)
	hosts := []string{}
	for _, service := range f.services {
		hosts = append(hosts, service.Self.Host)
	}
	for _, service := range f.services {
		if err := service.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		correct := true
		for _, node := range f.nodes {
			if err := node.Update(); err != nil {
				t.Fatal(err)
			}
			_, set, err := node.Get(collection, "@", true, nil, false)
			if err != nil || set == nil || set.Count() != volume || set.Abelian().Metric(0) != float64(volume*(volume-1)/2) {
				correct = false
			}
		}
		if correct {
			break
		}
		if time.Now().After(deadline) {
			for _, node := range f.nodes {
				_, cached, cachedErr := node.Get(collection, "@", true, nil, false)
				_, fresh, freshErr := node.Get(collection, "@", true, nil, true)
				t.Logf("node=%s ordinary=%v ordinaryErr=%v durable=%v durableErr=%v", node.Name(), cached, cachedErr, fresh, freshErr)
			}
			t.Fatal("ordinary reads did not converge after the continuous run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	snapshots, err := f.services[0].Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]storage.Record{}
	for _, snapshot := range snapshots {
		for _, row := range snapshot.Records {
			if _, duplicate := seen[row.ID]; duplicate || row.Deleted {
				t.Fatal("duplicate or unexpected tombstone")
			}
			seen[row.ID] = row
		}
	}
	for i := range volume {
		want := record(i)
		if got, ok := seen[want.ID]; !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("missing or changed acknowledged row %d: %v", i, got)
		}
	}
	if len(seen) != volume {
		t.Fatal("unexpected rows")
	}
	t.Logf("ack=%d successful_refreshed_reads=%d explicit_unavailable=%d concurrent_writer_sweeps=%d; final count=%d sum=%d", ack.Load(), reads.Load(), unavailable.Load(), sweeps.Load(), volume, volume*(volume-1)/2)
}

// Moving a vnode to another disk does not change XOR membership. The cheap
// refresh must still stop the old host from serving its derived summaries.
func TestSummaryOwnershipRefreshAfterPlacementOnlyMove(t *testing.T) {
	t.Parallel()
	for _, repush := range []bool{false, true} {
		t.Run(fmt.Sprintf("repush=%t", repush), func(t *testing.T) {
			root := VNodeDescriptor{ID: ID{255}}
			root.Name = BASE64.Encode(root.ID.Bytes())
			f := newReadPhaseFixture(t, []VNodeDescriptor{root, root}, true, false)
			const collection = "0000000000000001"
			stale := []*tree.View{f.nodes[0].mesh.summary.View(), f.nodes[1].mesh.summary.View()}
			if !f.nodes[0].mesh.summary.OwnsHere(collection, "0") || f.nodes[1].mesh.summary.OwnsHere(collection, "0") {
				t.Fatal("fixture must start with only the source owning summaries")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := f.services[1].ReceiveMove(ctx, f.services[0].Self.Host); err != nil {
				t.Fatal(err)
			}
			zone, err := NewZoneKey(collection, "0")
			if err != nil {
				t.Fatal(err)
			}
			for i, node := range f.nodes {
				snapshot, err := f.stores[i].CausalRouting()
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.Digest() != causal.Digest(stale[i].Digest) {
					t.Fatal("fixture changed membership instead of placement alone")
				}
				// Delay the derived cache only; the durable MOVE remains intact.
				// Before it, the placement was the source's (host 0).
				node.mesh.summary.SetView(stale[i], i == 0, false)
				if err := node.meshApplyViewIfChanged(repush); err != nil {
					t.Fatal(err)
				}
				if got, want := node.mesh.summary.OwnsHere(collection, "0"), i == 1; got != want {
					t.Fatalf("host %d summary ownership = %t, want %t", i, got, want)
				}
				contact, local, err := node.meshReadRoute(zone)
				if err != nil || local != (i == 1) {
					t.Fatalf("host %d route local=%t error=%v", i, local, err)
				}
				if (contact == node) != local {
					t.Fatalf("host %d confused shared vnode name with physical destination", i)
				}
			}
		})
	}
}

// Installed authority may legitimately differ from XOR placement after two
// stale joins or an explicit pairwise delegation. A derived accelerator must
// never turn the former owner's retained copy into a permanently stale total.
func TestDerivedReadsFollowOffPlacementNativeDelegation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	collection := "0000000000000001"
	zone, err := NewZoneKey(collection, "0")
	if err != nil {
		t.Fatal(err)
	}
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = BASE64.Encode(root.ID.Bytes())
	intended := VNodeDescriptor{ID: zone.ID}
	intended.Name = BASE64.Encode(intended.ID.Bytes())
	actual := VNodeDescriptor{ID: ID{128}}
	actual.Name = BASE64.Encode(actual.ID.Bytes())
	f := newReadPhaseFixture(t, []VNodeDescriptor{root, intended, actual}, false, false)
	first := storage.Record{Collection: collection, Location: "0", ID: "before", Metrics: []float64{2}}
	if _, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: first.ID, Record: &first}); err != nil {
		t.Fatal(err)
	}
	if err := f.services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	// The second join must plan against the first member's activation.
	// Syncing only after both joins updates membership, not installed rights.
	if err := f.services[2].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	if err := f.services[2].Join(ctx); err != nil {
		t.Fatal(err)
	}
	for _, service := range f.services {
		if err := service.Sync(ctx, []string{f.services[0].Self.Host, f.services[1].Self.Host, f.services[2].Self.Host}); err != nil {
			t.Fatal(err)
		}
	}
	region := tree.Region{Mask: ID{192}}
	rights, err := f.stores[1].Rights(region)
	if err != nil || len(rights) != 1 {
		t.Fatalf("expected intended owner's installed input: %+v %v", rights, err)
	}
	request := mesh.Request{Kind: "handoff", Input: rights[0].ID, Region: region, Target: f.services[2].Self, TargetVNode: actual}
	for {
		_, err := f.nodes[1].MeshData(ctx, request)
		if err == nil {
			break
		}
		if !errors.Is(err, mesh.ErrHandoffPending) {
			t.Fatal(err)
		}
	}
	second := storage.Record{Collection: collection, Location: "0", ID: "after", Metrics: []float64{7}}
	if _, err := f.nodes[2].MeshData(ctx, mesh.Request{Kind: "write", RequestID: second.ID, Record: &second}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.stores[1].Apply("old-writer", second); !errors.Is(err, storage.ErrNotOwner) {
		t.Fatal("former owner remained writable", err)
	}
	for _, node := range f.nodes {
		if err := node.Update(); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range f.nodes {
		_, fresh, err := node.Get(collection, "@", true, nil, true)
		if err != nil || fresh == nil || fresh.Count() != 2 || fresh.Abelian().Metric(0) != 9 {
			t.Fatalf("fresh node=%s set=%v err=%v", node.Name(), fresh, err)
		}
		_, ordinary, err := node.Get(collection, "@", true, nil, false)
		if err != nil || ordinary == nil || ordinary.Count() != 2 || ordinary.Abelian().Metric(0) != 9 {
			t.Fatalf("ordinary node=%s set=%v err=%v", node.Name(), ordinary, err)
		}
	}
	responders := 0
	for _, node := range f.nodes {
		values, err := node.GetAggregates(collection, []string{"@"})
		if err != nil {
			t.Fatal(err)
		}
		if total := values["@"]; total != nil {
			responders++
			if total.Count() != 2 || total.Metric(0) != 9 {
				t.Fatalf("aggregate node=%s retained an off-placement backup: %v", node.Name(), total)
			}
		}
	}
	if responders != 1 {
		t.Fatalf("aggregate had %d physical responders, want one", responders)
	}
}

// The parent's own native cell can be complete while a remote child's data
// moved away from its XOR owner. Refusing the child's exports without revoking
// the old contribution would leave the parent permanently one ACK behind.
func TestDerivedParentInvalidatesOffPlacementChildContribution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	collection := "0000000000000001"
	parentZone, _ := NewZoneKey(collection, "0")
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = BASE64.Encode(root.ID.Bytes())
	parent := VNodeDescriptor{ID: parentZone.ID}
	parent.Name = BASE64.Encode(parent.ID.Bytes())
	child := VNodeDescriptor{ID: ID{128}}
	child.Name = BASE64.Encode(child.ID.Bytes())
	f := newReadPhaseFixture(t, []VNodeDescriptor{root, parent, child}, false, false)
	location := BASE64.CharAt(32)
	first := storage.Record{Collection: collection, Location: location, ID: "before-child-delegation", Metrics: []float64{2}}
	if _, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: first.ID, Record: &first}); err != nil {
		t.Fatal(err)
	}
	for _, service := range f.services[1:] {
		if err := service.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, service := range f.services {
		if err := service.Sync(ctx, []string{f.services[0].Self.Host, f.services[1].Self.Host, f.services[2].Self.Host}); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range f.nodes {
		if err := node.Update(); err != nil {
			t.Fatal(err)
		}
	}
	_, before, err := f.nodes[1].Get(collection, "@", true, nil, false)
	if err != nil || before == nil || before.Count() != 1 {
		t.Fatal("parent did not hold the original complete child value", before, err)
	}
	region := tree.Region{Mask: ID{192}, Value: ID{128}}
	rights, err := f.stores[2].Rights(region)
	if err != nil || len(rights) != 1 {
		t.Fatalf("child input: %+v %v", rights, err)
	}
	request := mesh.Request{Kind: "handoff", Input: rights[0].ID, Region: region, Target: f.services[0].Self, TargetVNode: root}
	for {
		_, err := f.nodes[2].MeshData(ctx, request)
		if err == nil {
			break
		}
		if !errors.Is(err, mesh.ErrHandoffPending) {
			t.Fatal(err)
		}
	}
	second := storage.Record{Collection: collection, Location: location, ID: "after-child-delegation", Metrics: []float64{7}}
	if _, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: second.ID, Record: &second}); err != nil {
		t.Fatal(err)
	}
	for _, node := range f.nodes {
		if err := node.Update(); err != nil {
			t.Fatal(err)
		}
	}
	_, fresh, err := f.nodes[1].Get(collection, "@", true, nil, true)
	if err != nil || fresh == nil || fresh.Count() != 2 || fresh.Abelian().Metric(0) != 9 {
		t.Fatalf("fresh parent read lost a child ACK: %v %v", fresh, err)
	}
	_, ordinary, err := f.nodes[1].Get(collection, "@", true, nil, false)
	if err != nil || ordinary == nil || ordinary.Count() != 2 || ordinary.Abelian().Metric(0) != 9 {
		t.Fatalf("ordinary parent read retained an incomplete child: %v %v", ordinary, err)
	}
	values, err := f.nodes[1].GetAggregates(collection, []string{"@"})
	if err != nil || values["@"] == nil || values["@"].Count() != 2 || values["@"].Metric(0) != 9 {
		t.Fatalf("parent aggregate retained an incomplete child: %v %v", values, err)
	}
	third := storage.Record{Collection: collection, Location: location, ID: "after-invalidation", Metrics: []float64{5}}
	if _, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: third.ID, Record: &third}); err != nil {
		t.Fatal(err)
	}
	_, next, err := f.nodes[1].Get(collection, "@", true, nil, false)
	if err != nil || next == nil || next.Count() != 3 || next.Abelian().Metric(0) != 14 {
		t.Fatalf("invalidated parent lost the next child ACK: %v %v", next, err)
	}
	// Restore actual authority to its desired owner. Readiness must recover
	// from current state, without retaining a permanent unavailable-peer flag.
	rights, err = f.stores[0].Rights(region)
	if err != nil || len(rights) != 1 {
		t.Fatal("missing return input", rights, err)
	}
	request = mesh.Request{Kind: "handoff", Input: rights[0].ID, Region: region, Target: f.services[2].Self, TargetVNode: child}
	for {
		_, err := f.nodes[0].MeshData(ctx, request)
		if err == nil {
			break
		}
		if !errors.Is(err, mesh.ErrHandoffPending) {
			t.Fatal(err)
		}
	}
	for round := 0; round < 8 && !f.nodes[1].mesh.summary.ReadyFor(collection, "@"); round++ {
		for _, node := range f.nodes {
			if err := node.Update(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !f.nodes[1].mesh.summary.ReadyFor(collection, "@") {
		t.Fatal("derived cache did not recover after native placement stabilized")
	}
	_, restored, err := f.nodes[1].Get(collection, "@", true, nil, false)
	if err != nil || restored == nil || restored.Count() != 3 || restored.Abelian().Metric(0) != 14 {
		t.Fatalf("restored parent cache is incomplete: %v %v", restored, err)
	}
	for _, record := range []storage.Record{first, second, third} {
		record.Deleted = true
		if _, err := f.nodes[2].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "delete:" + record.ID, Record: &record}); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range f.nodes {
		if err := node.Update(); err != nil {
			t.Fatal(err)
		}
	}
	if !f.nodes[1].mesh.summary.ReadyFor(collection, "@") {
		t.Fatal("empty complete child did not restore cache readiness")
	}
	_, empty, err := f.nodes[1].Get(collection, "@", true, nil, false)
	if err != nil || empty == nil || empty.Count() != 0 {
		t.Fatal("complete zero child was not propagated", empty, err)
	}
}
