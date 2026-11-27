package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/http/p2p"
	"github.com/indexus/go-indexus-core/internal/adapter/peer"
	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/metrics"
)

// countingJobs counts the runs of each job of a worker and records the hold.
type countingJobs struct {
	observe, update, refresh, autoscale atomic.Int64
	held                                atomic.Bool
}

func (*countingJobs) Delay() time.Duration     { return time.Millisecond }
func (c *countingJobs) Observe() error         { c.observe.Add(1); return nil }
func (c *countingJobs) Update() error          { c.update.Add(1); return nil }
func (c *countingJobs) Refresh() error         { c.refresh.Add(1); return nil }
func (c *countingJobs) AutoscaleTick()         { c.autoscale.Add(1) }
func (c *countingJobs) HoldMaintenance(h bool) { c.held.Store(h) }
func (c *countingJobs) safeRuns() int64 {
	return c.observe.Load() + c.update.Load() + c.autoscale.Load()
}
func (c *countingJobs) allSafeRan() bool {
	return c.observe.Load() > 0 && c.update.Load() > 0 && c.autoscale.Load() > 0
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
	}
}

// suspendedJoin is a join that returns what the test sends it.
type suspendedJoin struct {
	jobs   *countingJobs
	worker *indexus.Worker
	right  atomic.Bool
	finish chan error
	result chan error
}

func suspendJoin(t *testing.T) *suspendedJoin {
	t.Helper()
	j := &suspendedJoin{jobs: &countingJobs{}, finish: make(chan error), result: make(chan error, 1)}
	j.worker = indexus.NewWorker(j.jobs)
	t.Cleanup(j.worker.Stop)
	go func() {
		j.result <- joinWithJobs(context.Background(), j.worker, j.right.Load, func(context.Context) error { return <-j.finish })
	}()
	return j
}

// The join is suspended. Without a right no job runs; from the first right
// the safe jobs run, the hold is on and nothing rebalances, while the join
// has not returned. Rebalance starts with Worker.Start, after the join.
func TestSafeJobsRunFromTheFirstRightWhileTheJoinIsSuspended(t *testing.T) {
	j := suspendJoin(t)
	time.Sleep(2 * firstRightPoll)
	if j.jobs.safeRuns() != 0 || j.jobs.refresh.Load() != 0 {
		t.Fatal("jobs ran before the vnode held a right")
	}
	j.right.Store(true)
	eventually(t, "safe jobs running during the join", j.jobs.allSafeRan)
	select {
	case err := <-j.result:
		t.Fatalf("the join returned: %v", err)
	default:
	}
	if j.jobs.refresh.Load() != 0 || !j.jobs.held.Load() {
		t.Fatalf("during the join: %d rebalances, maintenance held=%v", j.jobs.refresh.Load(), j.jobs.held.Load())
	}
	j.finish <- nil
	if err := <-j.result; err != nil {
		t.Fatal(err)
	}
	if j.jobs.refresh.Load() != 0 {
		t.Fatal("rebalance ran before Worker.Start")
	}
	go func() { _ = j.worker.Start() }()
	eventually(t, "rebalance after the join", func() bool { return j.jobs.refresh.Load() > 0 && !j.jobs.held.Load() })
}

// A join that fails stops the safe jobs before it is reported: none runs
// afterwards, and nothing rebalanced.
func TestFailedJoinStopsTheSafeJobs(t *testing.T) {
	j := suspendJoin(t)
	j.right.Store(true)
	eventually(t, "safe jobs running during the join", j.jobs.allSafeRan)
	failure := errors.New("receive move: source gone")
	j.finish <- failure
	if err := <-j.result; !errors.Is(err, failure) {
		t.Fatalf("join: %v", err)
	}
	runs := j.jobs.safeRuns()
	time.Sleep(30 * time.Millisecond)
	if j.jobs.safeRuns() != runs || j.jobs.refresh.Load() != 0 {
		t.Fatalf("after a failed join: %d more safe runs, %d rebalances", j.jobs.safeRuns()-runs, j.jobs.refresh.Load())
	}
}

// A join that ran out of time is not a failure: the vnode keeps running, and
// so do its safe jobs.
func TestUnfinishedJoinKeepsTheSafeJobs(t *testing.T) {
	j := suspendJoin(t)
	j.right.Store(true)
	eventually(t, "safe jobs running during the join", j.jobs.allSafeRan)
	j.finish <- fmt.Errorf("%w: deadline", errJoinUnfinished)
	if err := <-j.result; !errors.Is(err, errJoinUnfinished) {
		t.Fatalf("join: %v", err)
	}
	runs := j.jobs.safeRuns()
	eventually(t, "safe jobs still running", func() bool { return j.jobs.safeRuns() > runs })
}

// holdFirstHandoff lets the first handoff complete at the donor, which
// installs the fragment here, and then withholds its answer: the join is
// suspended with a first right installed.
type holdFirstHandoff struct {
	inner     mesh.Transport
	once      sync.Once
	installed chan struct{}
	release   chan struct{}
}

func (h *holdFirstHandoff) Exchange(ctx context.Context, host string, r mesh.Request) (mesh.Response, error) {
	out, err := h.inner.Exchange(ctx, host, r)
	if r.Kind == "handoff" && err == nil {
		h.once.Do(func() {
			close(h.installed)
			<-h.release
		})
	}
	return out, err
}

// A real vnode whose first fragment is installed while the rest of its join
// is suspended counts its items and ticks its autoscale; it runs no
// maintenance round before the join returns.
func TestVNodeWithAFirstFragmentRunsItsSafeJobsWhileItJoins(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	if err := source.node.New(&indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "record", Metrics: []float64{7}, RequestID: "put-1"}, "@", nil); err != nil {
		t.Fatal(err)
	}
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	config := Config{Name: indexus.BASE64.Encode(zone.Bytes()), Storage: filepath.Join(t.TempDir(), "node"), Advertise: []string{"127.0.0.1"},
		ClusterID: "integration", ClusterCapacity: 1 << 24, P2pPort: port,
		Bootstraps: []indexus.Contact{peer.NewContact(source.node.Name(), map[string]any{"127.0.0.1": nil}, source.port)}}
	settings, err := indexus.NewSettings(config.Name, port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	n, err := indexus.NewNode(settings, peer.NewContact, config.Bootstraps)
	if err != nil {
		t.Fatal(err)
	}
	n.EnableAutoscale(autoscale.Config{Enabled: true, ProvisionerURL: "http://127.0.0.1:9", StoragePath: t.TempDir()})
	server := p2p.NewHttpHandler("", n, peer.NewContact)
	boot, err := configureCluster(config, n, server, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hold := &holdFirstHandoff{inner: boot.mesh.Peers, installed: make(chan struct{}), release: make(chan struct{})}
	boot.mesh.Peers = hold
	go func() { _ = server.Serve(listener) }()
	t.Cleanup((&runningDataNode{node: n, boot: boot, server: server, port: port}).stop)
	jobs := indexus.NewWorker(n)
	t.Cleanup(jobs.Stop) // before the store closes

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := boot.Open(ctx, n); err != nil {
		t.Fatal(err)
	}
	rounds := metrics.Gossip.Rounds.Load()
	joined := make(chan error, 1)
	go func() {
		joined <- joinWithJobs(ctx, jobs, boot.hasRight, func(ctx context.Context) error { return boot.Join(ctx, n) })
	}()
	select {
	case <-hold.installed:
	case err := <-joined:
		t.Fatalf("the join returned before its first handoff: %v", err)
	case <-ctx.Done():
		t.Fatal("no first handoff")
	}
	eventually(t, "items counted and autoscale ticked while joining", func() bool {
		pressure, _ := n.AutoscaleSnapshot()["pressure"].(map[string]any)
		return n.Items() == 1 && len(pressure) > 0
	})
	if !n.Joining() || len(joined) != 0 {
		t.Fatal("the join was not suspended")
	}
	if got := metrics.Gossip.Rounds.Load(); got != rounds {
		t.Fatalf("%d maintenance rounds ran before the join returned", got-rounds)
	}
	close(hold.release)
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	go func() { _ = jobs.Start() }()
	eventually(t, "maintenance after the join", func() bool { return metrics.Gossip.Rounds.Load() > rounds })
}
