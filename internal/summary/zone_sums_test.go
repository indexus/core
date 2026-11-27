package summary

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// A flush reads zone totals through zoneSums, one read of a parent's
// children for all its siblings: every total must be the one the store
// gives, whether its parent was read before or not.
func TestZoneSumsMatchTheStore(t *testing.T) {
	peers := peerTransport{}
	node := describedService(t, "node", peers)
	peers[node.Self.Host] = node
	initCluster(t, node)
	tr := New(node, 1, Hooks{})
	const collection, alphabet = "Collection000000", "abAB"
	rng := rand.New(rand.NewSource(3))
	location := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	for i := range 400 {
		r := storage.Record{Collection: collection, Location: location(2 + rng.Intn(3)), ID: fmt.Sprint("r", i), Metrics: []float64{float64(i % 7)}}
		if _, _, _, err := node.Store.ApplyRecord(r.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	sums := tr.openZoneSums()
	defer sums.close()
	for range 300 {
		at := location(1 + rng.Intn(4))
		got, err := tr.zoneTotal(sums, collection, at)
		if err != nil {
			t.Fatal(err)
		}
		want, err := node.Store.ZoneTotal(collection, at)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("total of %q through sums: %+v, store: %+v", at, got, want)
		}
	}
	if len(sums.children) == 0 {
		t.Fatal("no parent read through sums")
	}
	// SUM-4: the sums keep the version they were opened at. A row committed
	// after it is not in any of them, until currentLocked moves them on.
	late := storage.Record{Collection: collection, Location: "ab", ID: "late", Metrics: []float64{1}}
	if _, _, _, err := node.Store.ApplyRecord(late.ID, late); err != nil {
		t.Fatal(err)
	}
	if root, err := tr.zoneTotal(sums, collection, tree.BASE64.Root()); err != nil || root.Count != 400 {
		t.Fatalf("root total %+v, %v after a later commit; want the 400 records of its version", root, err)
	}
	if ab, err := tr.zoneTotal(sums, collection, "ab"); err != nil {
		t.Fatal(err)
	} else if store, _ := node.Store.ZoneTotal(collection, "ab"); ab.Count != store.Count-1 {
		t.Fatalf("total of ab %d, store now %d: a later row leaked into the version", ab.Count, store.Count)
	}
	// Rows that reach the store without a local write go through
	// refreshLocked: sums read before it are dropped, not served.
	tr.Mu.Lock()
	gen := tr.storeGen
	tr.currentLocked(sums, &gen)
	kept := len(sums.children)
	tr.refreshLocked(tree.NewView(0))
	tr.currentLocked(sums, &gen)
	setAside := len(sums.retired)
	tr.Mu.Unlock()
	if kept == 0 || len(sums.children) != 0 {
		t.Fatalf("sums kept %d entries with no refresh, %d after one; want some, then none", kept, len(sums.children))
	}
	// Under Tree.Mu the old version is only set aside (closing a snapshot can
	// reclaim segments, file I/O); close releases it outside the lock.
	if setAside != 1 || sums.view != nil {
		t.Fatalf("currentLocked set aside %d versions, view open=%v; want 1 and closed", setAside, sums.view != nil)
	}
	root, err := tr.zoneTotal(sums, collection, tree.BASE64.Root())
	if err != nil || root.Count != 401 {
		t.Fatalf("root total %+v, %v; want the 401 records of the newer version", root, err)
	}
}

// The 64 siblings below one parent, valued as a flush values its edges:
// a version opened per sibling, or one for all through zoneSums.
func BenchmarkSiblingTotals(b *testing.B) {
	peers := peerTransport{}
	node := describedService(b, "bench", peers)
	if err := node.Store.InitCausal(node.VNode, causal.Genesis{Cluster: "test", Holder: node.Self}); err != nil {
		b.Fatal(err)
	}
	tr := New(node, 1, Hooks{})
	const collection = "Collection000000"
	for i := range 20000 {
		at := "A" + string(routingAlphabet[i%64]) + string(routingAlphabet[i/64%64])
		r := storage.Record{Collection: collection, Location: at, ID: fmt.Sprint("r", i)}
		if _, _, _, err := node.Store.ApplyRecord(r.ID, r); err != nil {
			b.Fatal(err)
		}
	}
	for _, mode := range []string{"store", "sums"} {
		b.Run(mode, func(b *testing.B) {
			for range b.N {
				shared := tr.openZoneSums()
				for c := range 64 {
					sums := shared
					if mode == "store" {
						sums = tr.openZoneSums()
					}
					if _, err := tr.zoneTotal(sums, collection, "A"+string(routingAlphabet[c])); err != nil {
						b.Fatal(err)
					}
					if mode == "store" {
						sums.close()
					}
				}
				shared.close()
			}
		})
	}
}
