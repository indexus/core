package mockup

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

func enableCausalSimulation(t *testing.T, network *Network, node *indexus.Node, join bool) {
	t.Helper()
	value, _ := durableMeshes.LoadOrStore(network, &durableNetwork{ready: map[string]bool{}})
	runtime := value.(*durableNetwork)
	store, err := storage.Open(filepath.Join(t.TempDir(), "causal.db"), node.Name(), "simulation", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	holder, err := store.Endpoint(fmt.Sprintf("http://%s:%d", node.Name(), node.Port()))
	if err != nil {
		t.Fatal(err)
	}
	service := &mesh.Service{Store: store, Self: holder, VNode: indexus.VNodeDescriptor{Name: node.Name(), ID: indexus.FromSlice(node.ID())}, Peers: &causalRPC{network: network, runtime: runtime, origin: node.Name()}}
	runtime.mu.Lock()
	var knowledge mesh.MembershipEnvelope
	if runtime.causalRoot == nil {
		runtime.causalRoot = service
		t.Cleanup(func() { durableMeshes.Delete(network) })
		knowledge = mesh.MembershipEnvelope{Root: service.VNode, Genesis: causal.Genesis{Cluster: "simulation", Holder: holder}}
	} else {
		knowledge.Root, knowledge.Genesis, knowledge.Delta, err = runtime.causalRoot.Store.Membership()
	}
	runtime.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.InitCausal(knowledge.Root, knowledge.Genesis); err != nil {
		t.Fatal(err)
	}
	if err = store.MergeMembership(knowledge.Delta); err != nil {
		t.Fatal(err)
	}
	if err = node.EnableCausalCluster(service); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.ready[node.Name()] = true
	runtime.mu.Unlock()
	if !join {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for {
		err = node.JoinCausalCluster(ctx)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			state, _ := store.CausalState()
			replica, _ := state.MembershipReplica()
			t.Logf("join diagnosis node=%s active=%v transfers=%d native=%d", node.Name(), replica.Current().Active, len(state.Transfers), len(state.Permissions.Native))
			for id, transfer := range state.Transfers {
				t.Logf("transfer %s from=%s to=%s frozen=%v emitted=%v installed=%v zones=%d", id, transfer.Cession.Source.VNode, transfer.Cession.Target.VNode, transfer.Phase >= causal.TransferMoving, transfer.Phase >= causal.TransferIssued, transfer.Phase == causal.TransferInstalled, len(transfer.Snapshots))
			}
			t.Fatalf("causal join %s: %v; last result %v", node.Name(), ctx.Err(), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type causalRPC struct {
	network *Network
	runtime *durableNetwork
	origin  string
}

func (r *causalRPC) Exchange(ctx context.Context, endpoint string, request mesh.Request) (mesh.Response, error) {
	if err := ctx.Err(); err != nil {
		return mesh.Response{}, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return mesh.Response{}, err
	}
	target, ok := r.network.GetFrom(r.origin, u.Hostname())
	if !ok {
		return mesh.Response{}, fmt.Errorf("mesh link partitioned")
	}
	r.runtime.mu.Lock()
	ready, after := r.runtime.ready[target.Name()], r.runtime.after
	r.runtime.mu.Unlock()
	if !ready {
		return mesh.Response{}, storage.ErrNotReady
	}
	transfer := request.Kind == "preload" || request.Kind == "mirror" || request.Kind == "stage" || request.Kind == "install" || request.Kind == "stage-zone" || request.Kind == "install-zone"
	if transfer && rand.Intn(100) < int(r.network.transferDropPct.Load()) {
		r.network.transferDropped.Add(1)
		return mesh.Response{}, fmt.Errorf("injected causal transfer drop")
	}
	out, err := target.MeshData(ctx, request)
	if transfer && err == nil {
		r.network.transferOK.Add(1)
	}
	if after != nil && request.Kind == "stage" && err == nil {
		after("freeze")
	}
	return out, err
}
