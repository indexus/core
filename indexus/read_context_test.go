package indexus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/wire"
)

type blockingPrefixTransport struct {
	mesh.Transport
	active, max atomic.Int64
	started     chan struct{}
}

func (p *blockingPrefixTransport) Exchange(ctx context.Context, host string, request mesh.Request) (mesh.Response, error) {
	if request.Kind != mesh.KindInventoryPrefix && request.Kind != "inventory" {
		return p.Transport.Exchange(ctx, host, request)
	}
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for {
		max := p.max.Load()
		if active <= max || p.max.CompareAndSwap(max, active) {
			break
		}
	}
	p.started <- struct{}{}
	<-ctx.Done()
	return mesh.Response{}, ctx.Err()
}
func TestReadContextsShareNodeBudgetAndCancelQueuedWork(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ingress := f.nodes[1]
	blocked := &blockingPrefixTransport{Transport: f.net, started: make(chan struct{}, 256)}
	f.services[1].Peers = blocked
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const callers = 256
	entered := make(chan struct{}, callers)
	failures := make(chan error, callers)
	var wait sync.WaitGroup
	for i := 0; i < callers; i++ {
		i := i
		wait.Add(1)
		go func() {
			defer wait.Done()
			entered <- struct{}{}
			var err error
			if i%2 == 0 {
				_, err = ingress.GetRecordsContext(ctx, "Collection000000", "aa", 100)
			} else {
				_, _, err = ingress.GetSetContext(ctx, "Collection000000", "aa", true, nil, true)
			}
			failures <- err
		}()
	}
	for range callers {
		<-entered
	}
	for range batchParallelism {
		select {
		case <-blocked.started:
		case <-time.After(3 * time.Second):
			t.Fatal("read workers did not start")
		}
	}
	select {
	case <-blocked.started:
		t.Fatal("more than node-wide read budget active")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	wait.Wait()
	close(failures)
	for err := range failures {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued/read cancellation: %v", err)
		}
	}
	if blocked.max.Load() != batchParallelism || blocked.active.Load() != 0 || len(ingress.mesh.readSlots) != 0 {
		t.Fatalf("read budget leaked: max=%d active=%d slots=%d", blocked.max.Load(), blocked.active.Load(), len(ingress.mesh.readSlots))
	}
}
func TestGetSetsReturnsNoBodyWhenAnyReadFails(t *testing.T) {
	t.Parallel()
	node := newNodeOn(t, 64)
	body, err := node.GetSetsBlockContext(context.Background(), "Demo000000000000", []string{"aa", "!"}, true, nil, true, nil, wire.SetsBlockF32, nil)
	if err == nil || body != nil {
		t.Fatalf("failed member presented partial success: body=%d err=%v", len(body), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if body, err := node.GetSetsBlockContext(ctx, "Demo000000000000", []string{"aa"}, true, nil, true, nil, wire.SetsBlockF32, nil); !errors.Is(err, context.Canceled) || body != nil {
		t.Fatalf("cancelled batch: body=%d err=%v", len(body), err)
	}
}

func TestReadContextCanCancelWhileColdSummaryRefreshIsBusy(t *testing.T) {
	t.Parallel()
	node := newNodeOn(t, 64)
	node.mesh.summary.RefreshMu.Lock()
	defer node.mesh.summary.RefreshMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, _, err := node.GetSetContext(ctx, "Demo000000000000", "aa", true, nil, false)
		finished <- err
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cold refresh cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read remained blocked behind refresh after cancellation")
	}
}

func TestCancelledRebuildKeepsVisibleSummary(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)
	if err := n.New(&Item{Collection: "Demo000000000000", Location: "aaa", Id: "row", Metrics: []float64{1}}, "@", nil); err != nil {
		t.Fatal(err)
	}
	if !n.mesh.summary.Ensure(context.Background()) {
		t.Fatal("not ready")
	}
	old := n.mesh.summary.Summaries
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := n.mesh.summary.FoldLocal(ctx, n.mesh.summary.View()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if n.mesh.summary.Summaries != old || n.mesh.summary.Owned("Demo000000000000", "@").Count() != 1 {
		t.Fatal("cancelled rebuild replaced live cache")
	}
}

func TestReadBatchRejectsOversizeAndDeduplicates(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)
	if body, err := n.GetSetsBlockContext(context.Background(), "Demo000000000000", make([]string, wire.MaxReadLocations+1), false, nil, false, nil, wire.SetsBlockF32, nil); err == nil || body != nil {
		t.Fatalf("unbounded batch: %v", err)
	}
	one, err := n.GetSetsBlockContext(context.Background(), "Demo000000000000", []string{"a"}, false, nil, false, nil, wire.SetsBlockF32, nil)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := n.GetSetsBlockContext(context.Background(), "Demo000000000000", []string{"a", " a ", "", "a"}, false, nil, false, nil, wire.SetsBlockF32, nil)
	if err != nil || string(repeated) != string(one) {
		t.Fatalf("duplicate work/result: %v", err)
	}
}
