package indexus

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type faultNet struct {
	mu      sync.RWMutex
	nodes   map[string]*Node
	cutHost atomic.Value // string: host isolated from all peers
}

func (n *faultNet) Exchange(ctx context.Context, host string, req mesh.Request) (mesh.Response, error) {
	if cut, _ := n.cutHost.Load().(string); cut != "" && cut == host {
		return mesh.Response{}, errReadPhasePartition
	}
	n.mu.RLock()
	target := n.nodes[host]
	n.mu.RUnlock()
	if target == nil {
		return mesh.Response{}, errReadPhasePartition
	}
	return target.MeshData(ctx, req)
}

// TestFaultUnderLoad_PartitionCrashRestartZeroLostACKs exercises REM-92
// palier 7 under -race: concurrent writes across 3 durable replicated nodes
// through (1) a network partition with fallback writes and degraded reads,
// (2) automatic P2P repair on heal, and (3) an abrupt node crash & disk
// restart. Every acknowledged write must survive with zero double owners.
func TestFaultUnderLoad_PartitionCrashRestartZeroLostACKs(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	net := &faultNet{nodes: map[string]*Node{}}
	var descriptors []VNodeDescriptor
	for _, first := range []byte{255, 127, 63} {
		id := ID{first, 1}
		descriptors = append(descriptors, VNodeDescriptor{ID: id, Name: BASE64.Encode(id.Bytes())})
	}
	paths := make([]string, len(descriptors))
	nodes := make([]*Node, len(descriptors))
	stores := make([]*storage.Store, len(descriptors))
	services := make([]*mesh.Service, len(descriptors))
	holders := make([]causal.Endpoint, len(descriptors))

	openNode := func(i int, initGenesis *causal.Genesis, selfGenesis bool) {
		settings, err := NewSettings(descriptors[i].Name, 9100+i, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		node, err := NewNode(settings, testNewContact, nil)
		if err != nil {
			t.Fatal(err)
		}
		store, err := storage.OpenWithOptions(paths[i], descriptors[i].Name, "fault-load", 1<<27, storage.Options{
			BatchWrites: true,
			BatchDelay:  time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		holder, err := store.Endpoint(fmt.Sprintf("http://127.0.0.1:%d", 9100+i))
		if err != nil {
			t.Fatal(err)
		}
		if selfGenesis {
			g := causal.Genesis{Cluster: "fault-load", Holder: holder}
			if err := store.InitCausal(descriptors[0], g); err != nil {
				t.Fatal(err)
			}
		} else if initGenesis != nil {
			if err := store.InitCausal(descriptors[0], *initGenesis); err != nil {
				t.Fatal(err)
			}
		}
		svc := &mesh.Service{Store: store, Self: holder, VNode: descriptors[i], Peers: net}
		if err := node.EnableCausalCluster(svc); err != nil {
			t.Fatal(err)
		}
		if err := node.EnableReplication(true, true); err != nil {
			t.Fatal(err)
		}
		net.mu.Lock()
		net.nodes[holder.Host] = node
		net.mu.Unlock()
		nodes[i], stores[i], services[i], holders[i] = node, store, svc, holder
	}

	for i := range descriptors {
		paths[i] = filepath.Join(t.TempDir(), "data.db")
	}
	// Open node 0 first to create genesis, then nodes 1 and 2.
	openNode(0, nil, true)
	genesis := causal.Genesis{Cluster: "fault-load", Holder: holders[0]}
	for i := 1; i < len(descriptors); i++ {
		openNode(i, &genesis, false)
	}
	t.Cleanup(func() {
		for _, s := range stores {
			if s != nil {
				_ = s.Close()
			}
		}
	})
	for i := 1; i < len(services); i++ {
		if err := services[i].Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range nodes {
		if err := n.meshMaintainContext(ctx); err != nil {
			t.Fatal(err)
		}
	}

	const col = "Fault00000000001"
	type keyRef struct{ loc, id string }
	var ackMu sync.Mutex
	acked := map[keyRef]float64{}
	recordAck := func(loc, id string, val float64) {
		ackMu.Lock()
		acked[keyRef{loc, id}] = val
		ackMu.Unlock()
	}

	// Phase 1: concurrent healthy writes across 18 zones.
	var seq atomic.Int64
	runBurst := func(n int) {
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < n/4; i++ {
					k := seq.Add(1)
					loc := tree.EncodeName(tree.ID{byte((k * 13) % 256)})
					id := fmt.Sprintf("k-%d", k)
					val := float64(k)
					rec := storage.Record{Collection: col, Location: loc, ID: id, Metrics: []float64{val}}
					if _, err := nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "w-" + id, Record: &rec}); err == nil {
						recordAck(loc, id, val)
					}
				}
			}()
		}
		wg.Wait()
	}

	runBurst(48)
	for _, n := range nodes {
		if err := n.meshMaintainContext(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 2: isolate node 1 (network partition). Pick a zone owned by node 1
	// and accept fallback writes on node 0 once T_suspect elapses.
	routing, err := stores[0].CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	var partZone tree.ZoneKey
	for b := 0; b < 256; b++ {
		z, err := tree.NewZoneKey(col, tree.EncodeName(tree.ID{byte(b)}))
		if err != nil {
			continue
		}
		h, err := routing.Route(z.ID)
		if err == nil && h == holders[1] {
			partZone = z
			break
		}
	}
	if partZone.Location == "" {
		t.Fatal("could not find a zone owned by node 1")
	}

	net.cutHost.Store(holders[1].Host)
	runBurst(24) // writes to nodes 0 and 2 continue under partition

	future := time.Now().Add(10 * time.Second)
	services[0].Now = func() time.Time { return future }
	fbRec := storage.Record{Collection: col, Location: partZone.Location, ID: "fb-during-partition", Metrics: []float64{999}}
	if _, err := nodes[0].MeshData(ctx, mesh.Request{
		Kind:      mesh.KindFallbackWrite,
		Owner:     holders[1],
		RequestID: "fb-part-1",
		Record:    &fbRec,
	}); err != nil {
		t.Fatalf("fallback write during partition: %v", err)
	}
	recordAck(partZone.Location, fbRec.ID, 999)

	// DegradedRead sees the fallback write while node 1 is partitioned.
	got, ok, err := nodes[0].DegradedRead(ctx, col, partZone.Location, fbRec.ID)
	if err != nil || !ok || got == nil || got.Metrics[0] != 999 {
		t.Fatalf("DegradedRead during partition = (%+v, %v, %v), want 999", got, ok, err)
	}

	// Phase 3: heal partition and run P2P maintenance -> automatic repair.
	net.cutHost.Store("")
	services[0].Now = nil
	for _, n := range nodes {
		if err := n.meshMaintainContext(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 4: abrupt crash & disk restart of node 2, then more writes & sync.
	net.mu.Lock()
	delete(net.nodes, holders[2].Host)
	net.mu.Unlock()
	if err := stores[2].Close(); err != nil {
		t.Fatal(err)
	}
	stores[2] = nil
	runBurst(24) // cluster continues serving writes while node 2 is down

	openNode(2, nil, false) // reopen exact same disk after crash
	runBurst(24)
	for round := 0; round < 2; round++ {
		for _, n := range nodes {
			if err := n.meshMaintainContext(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Final strict audit: 0 lost ACKs, 0 double owners, shadows caught up.
	routing, err = stores[0].CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if len(acked) < 60 {
		t.Fatalf("too few acked writes to be meaningful: %d", len(acked))
	}
	checkedZones := map[tree.ZoneKey]bool{}
	for k, wantVal := range acked {
		z, err := tree.NewZoneKey(col, k.loc)
		if err != nil {
			t.Fatal(err)
		}
		ownerHolder, err := routing.Route(z.ID)
		if err != nil {
			t.Fatal(err)
		}
		var ownersHolding int
		var foundVal float64
		var found bool
		for i, st := range stores {
			snap, err := st.Snapshot(z)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range snap.Records {
				if r.ID == k.id && !r.Deleted {
					ownersHolding++
					if holders[i] == ownerHolder {
						found = true
						foundVal = r.Metrics[0]
					}
				}
			}
		}
		if !found || foundVal != wantVal {
			t.Fatalf("lost or mismatched ACK for %s/%s: found=%v got=%v want=%v", k.loc, k.id, found, foundVal, wantVal)
		}
		if ownersHolding != 1 {
			t.Fatalf("double owner for %s/%s: held by %d stores", k.loc, k.id, ownersHolding)
		}
		if !checkedZones[z] {
			checkedZones[z] = true
			var ownerSeq uint64
			for i, st := range stores {
				if holders[i] == ownerHolder {
					snap, _ := st.Snapshot(z)
					ownerSeq = snap.Seq
				}
			}
			for i, st := range stores {
				if holders[i] == ownerHolder {
					continue
				}
				if sseq, ok, err := st.ShadowSeq(ownerHolder, z); err != nil || !ok || sseq < ownerSeq {
					t.Fatalf("shadow lag after heal on node %d for %s: got (%d,%v,%v), want >= %d", i, z.Location, sseq, ok, err, ownerSeq)
				}
			}
		}
	}
	t.Logf("PASS: %d acked writes audited across %d zones through partition + fallback + crash/restart (0 lost, 0 double owners, shadows synced)", len(acked), len(checkedZones))
}
