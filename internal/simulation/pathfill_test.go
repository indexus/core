package mockup

import (
	"context"
	"errors"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"path/filepath"
	"testing"
	"time"

	indexus "github.com/indexus/go-indexus-core/indexus"

	"github.com/indexus/go-indexus-core/internal/testoracle"
)

func TestQueueBackpressureOnNode(t *testing.T) {
	ResetNetwork()
	name, err := indexus.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := indexus.NewSettings(name, 21100, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	settings.SetAdvertise("127.0.0.1")
	n, err := indexus.NewNode(settings, NewContact, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Exercise the real bounded disk admission path. The old test expected
	// two volatile enqueues on a node that had no durable protocol initialized.
	store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "admission.db"), name, "simulation", 1<<24, storage.Options{BatchWrites: true, BatchSize: 8, MaxPendingBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	holder, _ := store.Endpoint("http://" + name + ":21100")
	descriptor := indexus.VNodeDescriptor{Name: name, ID: indexus.FromSlice(n.ID())}
	if err := store.InitCausal(descriptor, causal.Genesis{Cluster: "simulation", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	network.Join(n)
	rpc := &causalRPC{network: network, runtime: &durableNetwork{ready: map[string]bool{name: true}}, origin: name}
	if err := n.EnableCausalCluster(&mesh.Service{Store: store, Self: holder, VNode: descriptor, Peers: rpc}); err != nil {
		t.Fatal(err)
	}
	mk := func(id string) *indexus.Item {
		return &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: id, RequestID: "request:" + id, Metrics: []float64{1}}
	}
	if err := n.New(mk("a"), "@", nil); err != nil {
		t.Fatal(err)
	}
	oversized := mk("oversized")
	oversized.Metrics = make([]float64, storage.MaxMetrics) // valid, but more bytes than MaxPendingBytes
	if err := n.New(oversized, "@", nil); !errors.Is(err, storage.ErrWriteOverload) {
		t.Fatalf("node lost bounded disk admission refusal: %v", err)
	}
	if err := n.New(mk("b"), "@", nil); err != nil {
		t.Fatalf("refusal poisoned later writes: %v", err)
	}
	if err := n.New(mk("a"), "@", nil); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if count, err := n.Count(); err != nil || count != 2 {
		t.Fatalf("admission/replay changed data: count=%d err=%v", count, err)
	}

}

func TestPathFillFromOwnerNeighbor(t *testing.T) {
	ResetNetwork()

	n1 := newConvergenceNode(t, 8)
	n2 := newConvergenceNode(t, 8, n1)

	converge(t, []*indexus.Node{n1, n2}, 4, 2, 20)

	collection, err := indexus.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	loc := "aa"
	item := &indexus.Item{
		Collection: collection,
		Location:   loc,
		Id:         "x1",
		Metrics:    []float64{1, 2, 3, 4, 5},
	}
	if err := n1.New(item, indexus.BASE64.Root(), nil); err != nil {
		t.Fatal(err)
	}
	converge(t, []*indexus.Node{n1, n2}, 2, 2, 15)

	if got := totalCount(t, []*indexus.Node{n1, n2}); got != 1 {
		t.Fatalf("stored items = %d, want 1", got)
	}

	// The aggregate root and the leaf record are different XOR keys. Select
	// the root owner independently, never infer it from leaf data placement.
	rootID, err := indexus.ZoneID(collection, "@")
	if err != nil {
		t.Fatal(err)
	}
	owner, other := n1, n2
	if testoracle.XORCompare(rootID, [12]byte(n1.ID()), [12]byte(n2.ID())) > 0 {
		owner, other = n2, n1
	}
	if _, uncached, err := other.Get(collection, "@", false, nil, false); err != nil || uncached != nil {
		t.Fatalf("edge must initially be uncached: err=%v set=%v", err, uncached != nil)
	}

	_, ownedSet, err := owner.Get(collection, "@", false, nil, false)
	if err != nil || ownedSet == nil {
		t.Fatalf("owner must serve locally: err=%v set=%v", err, ownedSet != nil)
	}

	fresh, err := other.ReadContext(context.Background(), collection, "@", indexus.ReadOptions{Mode: indexus.ReadFresh})
	if err != nil || fresh.Set == nil || fresh.Set.Count() != 1 {
		t.Fatalf("fresh non-owner read: %+v %v", fresh, err)
	}
	cached, err := other.ReadContext(context.Background(), collection, "@", indexus.ReadOptions{Mode: indexus.ReadCached, MaxAge: time.Minute})
	if err != nil || cached.Set == nil || cached.Set.Count() != 1 || cached.Source != "cache" {
		t.Fatalf("explicit edge cache: %+v %v", cached, err)
	}
}
