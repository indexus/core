package mesh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// gateFS is a device whose writes wait while the gate is shut: a store that
// stopped serving, without any error.
type gateFS struct {
	zonelog.FS
	mu   sync.Mutex
	shut chan struct{}
}

func (f *gateFS) block() { f.mu.Lock(); f.shut = make(chan struct{}); f.mu.Unlock() }
func (f *gateFS) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shut != nil {
		close(f.shut)
		f.shut = nil
	}
}
func (f *gateFS) wait() {
	f.mu.Lock()
	shut := f.shut
	f.mu.Unlock()
	if shut != nil {
		<-shut
	}
}
func (f *gateFS) Create(name string) (zonelog.File, error) {
	file, err := f.FS.Create(name)
	return gateFile{file, f}, err
}

type gateFile struct {
	zonelog.File
	fs *gateFS
}

func (f gateFile) Write(p []byte) (int, error) { f.fs.wait(); return f.File.Write(p) }

func gatedHolder(t *testing.T) (*Service, *gateFS) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.db")
	if err := os.MkdirAll(storage.RecordsDir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	gate := &gateFS{FS: zonelog.Dir(storage.RecordsDir(path))}
	store, err := storage.OpenWithOptions(path, "node-0", "test", 1<<26, storage.Options{NoSync: true, FS: gate})
	if err != nil {
		t.Fatal(err)
	}
	holder, _ := store.Endpoint("node-0")
	s := &Service{Store: store, Self: holder, VNode: tree.VNodeDescriptor{Name: "node-0", ID: tree.ID{1, 1}}}
	t.Cleanup(func() { gate.release(); s.Close(); store.Close() })
	if err := store.InitCausal(s.VNode, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	return s, gate
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
	}
}

// AWS 2026-11-26: the store blocked, acknowledgements fell and the scale-up
// signal fell with them. Demand is now counted on arrival: writes that wait
// stay visible and keep the controller hot while nothing is acknowledged.
func TestBlockedStoreKeepsDemandVisibleAndControllerHot(t *testing.T) {
	holder, gate := gatedHolder(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	write := func(i int) error {
		record := storage.Record{Collection: "collection-00000", Location: "aa", ID: fmt.Sprintf("blocked-%d", i)}
		_, err := holder.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("blocked-%d", i), Record: &record})
		return err
	}
	if err := write(0); err != nil {
		t.Fatal(err)
	}
	served := holder.Writes.Sample(time.Now())
	if served.Arrived != 1 || served.Applied != 1 || served.Acknowledged != 1 || served.InFlight != 0 || served.OldestWait != 0 {
		t.Fatalf("a served write: %+v", served)
	}
	servedDemand := holder.Demand.Load()

	gate.block()
	const waiting = 8
	var done sync.WaitGroup
	for i := 1; i <= waiting; i++ {
		done.Add(1)
		go func() { defer done.Done(); _ = write(i) }()
	}
	waitFor(t, "writes in flight", func() bool { return holder.Writes.InFlight.Load() == waiting })

	later := time.Now().Add(3 * time.Second)
	blocked := holder.Writes.Sample(later)
	if blocked.Arrived != served.Arrived+waiting {
		t.Fatalf("arrivals=%d, want %d: demand must not wait for the store", blocked.Arrived, served.Arrived+waiting)
	}
	if blocked.Acknowledged != served.Acknowledged || blocked.Applied != served.Applied || holder.Demand.Load() != servedDemand {
		t.Fatalf("a blocked store acknowledged: %+v", blocked)
	}
	if blocked.OldestWait < 3*time.Second || blocked.OldestWait > 8*time.Second {
		t.Fatalf("oldest wait=%s, want the age of the first blocked write", blocked.OldestWait)
	}

	// The controller reads what Node.AutoscaleTick gives it: the arrival rate,
	// the writes in flight and the oldest wait. Arrivals stop too once every
	// client waits; the wait alone must keep it hot.
	sent := make(chan autoscale.Request, 1)
	controller := autoscale.New(autoscale.Config{Enabled: true, DisableDiskSignal: true, WritesLimit: 500, Now: func() time.Time { return later }})
	controller.Tick(autoscale.Input{
		SelfName:   "selfNode000000000000000000000000",
		WriteRate:  0,
		Queue:      int(blocked.InFlight),
		OldestWait: blocked.OldestWait,
		Resources:  autoscale.ResourceSample{DiskFreePct: 80, DiskOK: true},
	}, func(req autoscale.Request) error { sent <- req; return nil })
	select {
	case req := <-sent:
		if req.Reason != "queue" {
			t.Fatalf("reason=%q, want queue", req.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("no acknowledgement, writes waiting 3s, and the controller is not hot")
	}

	gate.release()
	done.Wait()
	after := holder.Writes.Sample(time.Now())
	if after.InFlight != 0 || after.OldestWait != 0 || after.Acknowledged != served.Acknowledged+waiting || after.Refused != 0 {
		t.Fatalf("after the store resumed: %+v", after)
	}
}

// A relay hop is not an arrival at the relay, and the relay's retry of a
// request the holder refused at a cutover is the same arrival.
func TestRelayedAndRetriedWritesArriveOnceAtTheHolder(t *testing.T) {
	_, source, target := streamFixture(t, 64)
	ctx := context.Background()
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	zone, err := tree.NewZoneKey("collection-00000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	relayBefore, holderBefore := source.Writes.Sample(time.Now()), target.Writes.Sample(time.Now())

	record := storage.Record{Collection: zone.Collection, Location: zone.Location, ID: "relayed"}
	out, err := source.Handle(ctx, Request{Kind: "write", RequestID: "relayed", Record: &record})
	if err != nil || out.AppliedBy.Owner != target.Self.Owner {
		t.Fatalf("the write must be relayed to the holder: appliedBy=%v err=%v", out.AppliedBy, err)
	}
	relay, holder := source.Writes.Sample(time.Now()), target.Writes.Sample(time.Now())
	if relay.Arrived != relayBefore.Arrived || relay.Forwarded != relayBefore.Forwarded+1 || relay.InFlight != 0 || relay.Acknowledged != relayBefore.Acknowledged {
		t.Fatalf("the relay counted the hop as its own: before %+v after %+v", relayBefore, relay)
	}
	if holder.Arrived != holderBefore.Arrived+1 || holder.Acknowledged != holderBefore.Acknowledged+1 {
		t.Fatalf("relayed write at the holder: before %+v after %+v", holderBefore, holder)
	}

	// The holder is in a cutover of this lot that outlasts cutoverWaitLimit: it
	// refuses (frozen). callWrite retries the same request on the same endpoint.
	barrier := make(chan struct{})
	target.cutoverMu.Lock()
	target.cutovers = map[tree.Key]chan struct{}{zone.Key(): barrier}
	target.cutoverMu.Unlock()
	go func() {
		// Lift the barrier after the first refusal (or give up: the test then fails on its counts).
		for deadline := time.Now().Add(5 * time.Second); target.Writes.Refused[refusedFrozen].Load() == 0 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		target.cutoverMu.Lock()
		delete(target.cutovers, zone.Key())
		target.cutoverMu.Unlock()
		close(barrier)
	}()
	retried := storage.Record{Collection: zone.Collection, Location: zone.Location, ID: "retried"}
	out, err = source.Handle(ctx, Request{Kind: "write", RequestID: "retried", Record: &retried})
	if err != nil || out.AppliedBy.Owner != target.Self.Owner {
		t.Fatalf("the retried write must be applied by the holder: appliedBy=%v err=%v", out.AppliedBy, err)
	}
	again := target.Writes.Sample(time.Now())
	if again.Retried == holder.Retried {
		t.Fatal("the holder saw no retry: the test proves nothing")
	}
	if again.Arrived != holder.Arrived+1 {
		t.Fatalf("a request retried %d times arrived %d times at the holder", again.Retried-holder.Retried, again.Arrived-holder.Arrived)
	}
	if again.RefusedBy["frozen"] != holder.RefusedBy["frozen"]+1 || again.Acknowledged != holder.Acknowledged+1 || again.InFlight != 0 {
		t.Fatalf("outcomes of the retried write: before %+v after %+v", holder, again)
	}
}

// The hot path stays free of allocation and the oldest wait stays bounded.
func TestWriteDemandTracksOldestWaitWithoutAllocating(t *testing.T) {
	var d WriteDemand
	base := time.Unix(1_800_000_000, 0)
	first := d.arrive("first", base.UnixNano())
	visits := make([]writeVisit, 0, 2*waitSlots)
	for i := range 2 * waitSlots {
		visits = append(visits, d.arrive("later", base.Add(time.Duration(i+1)*time.Millisecond).UnixNano()))
	}
	if got := d.OldestWait(base.Add(5 * time.Second)); got != 5*time.Second {
		t.Fatalf("oldest wait=%s with more writes in flight than slots, want 5s", got)
	}
	first.finish(nil)
	if got := d.OldestWait(base.Add(5 * time.Second)); got != 5*time.Second-time.Millisecond {
		t.Fatalf("oldest wait=%s after the oldest left", got)
	}
	for i := range visits {
		visits[i].finish(nil)
	}
	if d.InFlight.Load() != 0 || d.OldestWait(base.Add(time.Hour)) != 0 {
		t.Fatalf("in flight=%d oldest=%s after every write left", d.InFlight.Load(), d.OldestWait(base.Add(time.Hour)))
	}
	now := base.UnixNano()
	if allocs := testing.AllocsPerRun(100, func() {
		v := d.arrive("request-id", now)
		v.forward()
		v.comeBack(now)
		v.finish(nil)
	}); allocs != 0 {
		t.Fatalf("%g allocations per write", allocs)
	}
}
