package mesh

import (
	"context"
	"sync"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// REM-87: copy and per-zone requests of a join carry the certificate and
// their snapshots, never the per-zone states of the whole operation, which
// made every call of a split O(zones) on the wire.
func TestTransferRequestsCarryNoZoneStates(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	ctx := context.Background()
	view := tree.NewView(0)
	_ = view.Add(nodes[0].VNode.Name, nodes[0].VNode.ID)
	_ = view.Add(nodes[1].VNode.Name, nodes[1].VNode.ID)
	for _, loc := range []string{"W", "X", "m", "n", "aa", "ab", "B", "C"} {
		r := storage.Record{Collection: "Probe00000000000", Location: loc, ID: "row", Metrics: []float64{1}}
		if _, err := nodes[0].Write(ctx, "seed-"+loc, r); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	net.setBefore(func(request Request) error {
		if request.Kind == "install-zone" {
			mu.Lock()
			seen[request.Kind]++
			if request.Transfer != nil || len(request.Seals) == 0 {
				t.Error("final certification resent records instead of seals")
			}
			mu.Unlock()
		}
		if request.Transfer != nil && request.Kind == "preload" {
			mu.Lock()
			seen[request.Kind]++
			mu.Unlock()
		}
		return nil
	})
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if seen["preload"] == 0 || seen["install-zone"] == 0 {
		t.Fatalf("join sent preload=%d install-zone=%d, want both", seen["preload"], seen["install-zone"])
	}
}
