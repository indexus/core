package mesh

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type gatedInventoryTransport struct {
	started     chan string
	release     chan struct{}
	active, max atomic.Int64
	coverage    map[string]tree.Region
}

func (p *gatedInventoryTransport) Exchange(ctx context.Context, host string, r Request) (Response, error) {
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for {
		old := p.max.Load()
		if old >= active || p.max.CompareAndSwap(old, active) {
			break
		}
	}
	select {
	case p.started <- host:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	return Response{Page: &storage.InventoryPage{Watermark: 1, Done: true, Coverage: []tree.Region{p.coverage[host]}}, Inventory: []storage.Snapshot{{Zone: tree.ZoneKey{Collection: host, Location: "a"}}}}, nil
}
func TestQueryHostFanoutIsBoundedAndResultsRetainCoverageOrder(t *testing.T) {
	t.Parallel()
	_, nodes := meshFixture(t, 1)
	transport := &gatedInventoryTransport{started: make(chan string, 16), release: make(chan struct{}), coverage: map[string]tree.Region{}}
	nodes[0].Peers = transport
	var holders []causal.Endpoint
	for _, host := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		holders = append(holders, causal.Endpoint{Owner: causal.Owner{VNode: host, Storage: host}, Host: host})
	}
	view := tree.NewView(0)
	for i, h := range holders {
		if err := view.Add(h.VNode, tree.ID{byte(1 + i*24)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range holders {
		region, err := tree.Cell(view, h.VNode)
		if err != nil {
			t.Fatal(err)
		}
		transport.coverage[h.Host] = region
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		rows, err := nodes[0].queryHolders(ctx, holders, Request{Kind: "inventory"})
		if err == nil {
			for i, row := range rows {
				if row.Zone.Collection != holders[i].Host {
					t.Error("coverage order changed")
				}
			}
			if len(rows) != len(holders) {
				t.Error("missing holder")
			}
		}
		done <- err
	}()
	for range 4 {
		select {
		case <-transport.started:
		case <-ctx.Done():
			t.Fatal("host scans did not overlap")
		}
	}
	select {
	case <-transport.started:
		t.Fatal("fanout exceeded four")
	case <-time.After(10 * time.Millisecond):
	}
	close(transport.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if transport.max.Load() != 4 || transport.active.Load() != 0 {
		t.Fatal("fanout budget leaked")
	}
}
