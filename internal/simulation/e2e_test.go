package mockup

import (
	"context"
	"fmt"
	"testing"
	"time"

	indexus "github.com/indexus/go-indexus-core/indexus"
)

const spreadChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

func spreadLocation(i int) string {
	return string([]byte{spreadChars[i%len(spreadChars)], spreadChars[(i/len(spreadChars))%len(spreadChars)]})
}

// End-to-end edge absorb: clients with different routing keys write and delete
// through whichever node is nearest to them, reads are served from edge caches
// after a single path-fill, and no item is counted twice along the way.
func TestEdgeAbsorbReadWriteDeleteEndToEnd(t *testing.T) {
	ResetNetwork()

	const (
		items      = 120
		deletions  = 40
		delegation = 25
	)
	root := indexus.BASE64.Root()

	n1 := newConvergenceNode(t, delegation)
	n2 := newConvergenceNode(t, delegation, n1)
	n3 := newConvergenceNode(t, delegation, n1)
	nodes := []*indexus.Node{n1, n2, n3}

	converge(t, nodes, 4, 2, 30)

	collection, err := indexus.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}

	// Each write enters on a different edge, as clients with distinct routing
	// keys would. None of them targets the owner on purpose.
	made := make([]*indexus.Item, 0, items)
	for i := 0; i < items; i++ {
		item := &indexus.Item{
			Collection: collection,
			Location:   spreadLocation(i),
			Id:         fmt.Sprintf("id-%d", i),
			Metrics:    []float64{1, 2, 3, 4, 5},
		}
		made = append(made, item)
		if err := nodes[i%len(nodes)].New(item, root, nil); err != nil {
			t.Fatalf("New on edge %d: %v", i%len(nodes), err)
		}
	}

	converge(t, nodes, 3, 2, 30)

	if total := totalCount(t, nodes); total != items {
		t.Fatalf("items lost or double counted after ingress: got %d want %d", total, items)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	// Routing-only Get(deep=false) is not a cache policy. Warm an exhaustive
	// result, then explicitly require the bounded edge cache to serve it.
	edges := 0
	for _, n := range nodes {
		if _, set, _ := n.Get(collection, root, false, nil, false); set != nil {
			continue // this node owns the root or already cached it
		}
		fresh, err := n.ReadContext(context.Background(), collection, root, indexus.ReadOptions{Mode: indexus.ReadFresh})
		if err != nil || fresh.Set == nil || fresh.Set.Count() != items {
			t.Fatalf("fresh fill on %s: %+v %v", n.Name(), fresh, err)
		}
		cached, err := n.ReadContext(context.Background(), collection, root, indexus.ReadOptions{Mode: indexus.ReadCached, MaxAge: time.Minute})
		if err != nil || cached.Set == nil || cached.Set.Count() != items || cached.Source != "cache" {
			t.Fatalf("edge %s did not serve its bounded cache: %+v %v", n.Name(), cached, err)
		}
		edges++
	}
	if edges == 0 {
		t.Fatal("no non-owner edge exercised, the cache assertion proved nothing")
	}

	// Deletions take the same edges as the writes did.
	for i := 0; i < deletions; i++ {
		item := made[i]
		if err := nodes[i%len(nodes)].Delete(item, root, nil); err != nil {
			t.Fatalf("Delete on edge %d: %v", i%len(nodes), err)
		}
	}

	converge(t, nodes, 3, 2, 30)

	want := items - deletions
	if total := totalCount(t, nodes); total != want {
		t.Fatalf("count after deletions: got %d want %d", total, want)
	}
	assertOwnershipDisjointAndComplete(t, nodes)
	for _, n := range nodes {
		fresh, err := n.ReadContext(context.Background(), collection, root, indexus.ReadOptions{Mode: indexus.ReadFresh})
		if err != nil || fresh.Set == nil || fresh.Set.Count() != want {
			t.Fatalf("fresh deletion visibility on %s: %+v %v", n.Name(), fresh, err)
		}
	}

	// Replaying the same deletions must be a no-op, not a second subtraction.
	for i := 0; i < deletions; i++ {
		item := made[i]
		if err := nodes[i%len(nodes)].Delete(item, root, nil); err != nil {
			t.Fatalf("repeated Delete on edge %d: %v", i%len(nodes), err)
		}
	}

	if total := totalCount(t, nodes); total != want {
		t.Fatalf("repeated deletions changed the count: got %d want %d", total, want)
	}
}
