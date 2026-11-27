package mesh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestFirstLotWritesBeforeRemainingRegionIsCopied(t *testing.T) {
	t.Parallel()
	const count = 1100
	net, source, target := streamFixture(t, count)
	ctx := context.Background()
	copied := map[string]bool{}
	var mu sync.Mutex
	var first sync.Once
	probed := false
	net.setBefore(func(request Request) error {
		if request.Kind == "preload" {
			mu.Lock()
			for _, snap := range request.Transfer.Snapshots {
				copied[zoneKey(snap)] = true
			}
			mu.Unlock()
		}
		return nil
	})
	net.setAfter(func(request Request) error {
		if request.Kind != "install-zone" {
			return nil
		}
		var failed error
		first.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			probed = true
			if len(copied) >= count {
				failed = fmt.Errorf("all %d zones were copied before the first authority flip", count)
				return
			}
			z := request.ZoneTransfers[0].Zone
			r := storage.Record{Collection: z.Collection, Location: z.Location, ID: "early-write", Metrics: []float64{1}}
			out, err := target.Handle(ctx, Request{Kind: "write", RequestID: "early-write", Record: &r})
			if err != nil || !out.AppliedLocally || out.AppliedBy != target.Self {
				failed = fmt.Errorf("first lot not writable on receiver: %v", err)
				return
			}
			if _, _, err := source.Store.Apply("forbidden-old-writer", r); err == nil {
				failed = fmt.Errorf("donor still acknowledges the installed lot")
			}
		})
		return failed
	})
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if !probed {
		t.Fatal("no per-lot install exercised")
	}
	rows, err := target.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	records := 0
	for _, snap := range rows {
		records += len(snap.Records)
	}
	if records != count+1 {
		t.Fatalf("recovered %d rows, want all seeds and the early ACK", records)
	}
}

func TestGranularScaleKeepsACKsAndOneWriterAcrossVnodes(t *testing.T) {
	t.Parallel()
	count := 8
	if testing.Short() {
		count = 4
	}
	if value := os.Getenv("INDEXUS_SCALE_VNODES"); value != "" {
		var err error
		count, err = strconv.Atoi(value)
		if err != nil || count < 2 || count > 128 {
			t.Fatal("INDEXUS_SCALE_VNODES must be 2..128")
		}
	}
	zones := 256
	if value := os.Getenv("INDEXUS_SCALE_ZONES"); value != "" {
		var err error
		zones, err = strconv.Atoi(value)
		if err != nil || zones < 32 || zones > 262144 {
			t.Fatal("INDEXUS_SCALE_ZONES must be 32..262144")
		}
	}
	net, nodes := meshFixture(t, count)
	for i := 1; i < count; i++ {
		nodes[i].VNode.ID = tree.ID{byte(1 + i*29), 1}
	}
	errLostInstallACK := errors.New("injected lost install ACK")
	var lost sync.Map
	if os.Getenv("INDEXUS_SCALE_LOSE_INSTALL_ACK") == "1" {
		net.setAfter(func(request Request) error {
			if request.Kind == "install-zone" {
				if _, already := lost.LoadOrStore(request.Operation, true); !already {
					return errLostInstallACK
				}
			}
			return nil
		})
	}
	timeout := count * 5
	if value := os.Getenv("INDEXUS_SCALE_TIMEOUT_SECONDS"); value != "" {
		var err error
		timeout, err = strconv.Atoi(value)
		if err != nil || timeout < 1 || timeout > 1800 {
			t.Fatal("INDEXUS_SCALE_TIMEOUT_SECONDS must be 1..1800")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	acked := map[string]bool{}
	var ackMu sync.Mutex
	apply := func(id string, i int) error {
		r := storage.Record{Collection: "Demo000000000000", Location: string([]byte{alphabet[i%64], alphabet[(i/64)%64], alphabet[(i/4096)%64]}), ID: id, Metrics: []float64{1}}
		if _, err := nodes[0].Write(ctx, id, r); err != nil {
			return err
		}
		ackMu.Lock()
		acked[id] = true
		ackMu.Unlock()
		return nil
	}
	retryApply := func(id string, i int) error {
		for {
			err := apply(id, i)
			if err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) && !errors.Is(err, storage.ErrWriteOverload) {
				return err
			}
			time.Sleep(time.Millisecond)
		}
	}
	write := func(id string, i int) {
		t.Helper()
		if err := retryApply(id, i); err != nil {
			t.Fatal(err)
		}
	}
	for i := range zones {
		write(fmt.Sprintf("seed-%d", i), i)
	}
	stop, stopped := make(chan struct{}), make(chan error, 1)
	if os.Getenv("INDEXUS_SCALE_CONCURRENT") == "1" {
		go func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					stopped <- nil
					return
				default:
				}
				location := i * 7919 % zones
				if os.Getenv("INDEXUS_SCALE_NEW_ZONES") == "1" && i < zones/2 {
					location = zones + i
				}
				for {
					err := apply(fmt.Sprintf("concurrent-%d", i), location)
					if err == nil {
						break
					}
					if ctx.Err() != nil {
						stopped <- ctx.Err()
						return
					}
					if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) && !errors.Is(err, storage.ErrWriteOverload) {
						stopped <- err
						return
					}
					time.Sleep(time.Millisecond)
				}
				time.Sleep(time.Millisecond)
			}
		}()
	} else {
		stopped <- nil
	}
	var stopOnce sync.Once
	stopWrites := func() { stopOnce.Do(func() { close(stop) }) }
	defer stopWrites()
	started := time.Now()
	parallel := 1
	if value := os.Getenv("INDEXUS_SCALE_PARALLEL"); value != "" {
		var err error
		parallel, err = strconv.Atoi(value)
		if err != nil || parallel < 1 || parallel > 8 {
			t.Fatal("INDEXUS_SCALE_PARALLEL must be 1..8")
		}
	}
	jobs := make(chan int)
	failures := make(chan error, count)
	var joins sync.WaitGroup
	for range parallel {
		joins.Go(func() {
			for i := range jobs {
				var joinErr error
				for {
					joinErr = nodes[i].Join(ctx)
					if joinErr == nil || !errors.Is(joinErr, errLostInstallACK) || ctx.Err() != nil {
						break
					}
				}
				if joinErr != nil {
					failures <- joinErr
					continue
				}
				for j := range 16 {
					if err := retryApply(fmt.Sprintf("after-%d-%d", i, j), i*31+j); err != nil {
						failures <- err
						break
					}
				}
			}
		})
	}
	for i := 1; i < count; i++ {
		jobs <- i
	}
	close(jobs)
	joins.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}

	stopWrites()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	hosts := make([]string, count)
	for i, node := range nodes {
		hosts[i] = node.Self.Host
	}
	for range 2 {
		for _, node := range nodes {
			if err := node.Sync(ctx, hosts); err != nil {
				t.Fatal(err)
			}
			if err := node.Rebalance(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := nodes[0].Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, snap := range rows {
		for _, r := range snap.Records {
			seen[r.ID]++
		}
	}
	for id := range acked {
		if seen[id] != 1 {
			t.Fatalf("ACK %s present %d times", id, seen[id])
		}
	}
	t.Logf("vnodes=%d zones=%d ACKs=%d observed_rows=%d join_and_audit=%v concurrent=%s parallel=%d lost_install_ack=%s new_zones=%s", count, zones, len(acked), len(seen), time.Since(started), os.Getenv("INDEXUS_SCALE_CONCURRENT"), parallel, os.Getenv("INDEXUS_SCALE_LOSE_INSTALL_ACK"), os.Getenv("INDEXUS_SCALE_NEW_ZONES"))
	for i := range 32 {
		r := storage.Record{Collection: "Demo000000000000", Location: string([]byte{alphabet[i*2], '0', '0'}), ID: fmt.Sprintf("authority-probe-%d", i), Metrics: []float64{1}}
		writers := 0
		for _, node := range nodes {
			if _, _, err := node.Store.Apply(r.ID, r); err == nil {
				writers++
			}
		}
		if writers != 1 {
			t.Fatalf("zone %s has %d ACK writers", r.Location, writers)
		}
	}
}

// Unified FLIP-UNIT + granular copy: the hottest unit copies, flips and accepts
// writes on the receiver BEFORE the rest of the region is even copied.
func TestGranularHandoffFlipsHottestUnitFirstBeforeRegionCopies(t *testing.T) {
	t.Parallel()
	const count = 256
	net, source, target := streamFixture(t, count)
	hotKey := tree.Key{Collection: fmt.Sprintf("collection-%05d", count-1), Location: "aa"}
	source.Heat = func(k tree.Key) int64 {
		if k == hotKey {
			return 1000
		}
		return 0
	}
	ctx := context.Background()
	copied := map[string]bool{}
	var mu sync.Mutex
	var first sync.Once
	var hotFirst bool
	net.setBefore(func(request Request) error {
		if request.Kind == "preload" && request.Transfer != nil {
			mu.Lock()
			for _, snap := range request.Transfer.Snapshots {
				copied[zoneKey(snap)] = true
			}
			mu.Unlock()
		}
		return nil
	})
	net.setAfter(func(request Request) error {
		if request.Kind != "install-zone" {
			return nil
		}
		var failed error
		first.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			if len(copied) >= count {
				failed = fmt.Errorf("all %d zones copied before hottest unit flipped", count)
				return
			}
			for _, zt := range request.ZoneTransfers {
				if zt.Zone.Key() == hotKey {
					hotFirst = true
					break
				}
			}
			if !hotFirst {
				failed = fmt.Errorf("first installed lot did not contain hot zone %v", hotKey)
				return
			}
			r := storage.Record{Collection: hotKey.Collection, Location: hotKey.Location, ID: "hot-early-write", Metrics: []float64{42}}
			out, err := target.Handle(ctx, Request{Kind: "write", RequestID: "hot-early-write", Record: &r})
			if err != nil || !out.AppliedLocally || out.AppliedBy != target.Self {
				failed = fmt.Errorf("hot zone not writable on receiver after first flip: %+v %v", out, err)
			}
		})
		return failed
	})
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if !hotFirst {
		t.Fatal("first lot install never observed")
	}
	rows, err := target.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, snap := range rows {
		for _, r := range snap.Records {
			if r.ID == "hot-early-write" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("early write on hot zone lost after consolidation")
	}
}

// Mid-lot fault recovery: a lot cut after FreezeZones finishes first on retry,
// and a lost install-zone ACK replayed via resumeZoneInstalls never overwrites
// live writes already acknowledged on the receiver.
func TestGranularHandoffResumesFrozenAndLostACKWithoutOverwritingLiveWrites(t *testing.T) {
	t.Parallel()
	net, source, target := streamFixture(t, 128)
	source.FlipUnitZones = 32
	ctx := context.Background()

	var stageFail, installFail atomic.Bool
	var frozenFirst tree.ZoneKey
	var liveZone tree.ZoneKey
	var mu sync.Mutex
	var firstResumedHasFrozen bool
	var checkResumed atomic.Bool

	source.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if checkResumed.Load() && r.Kind == KindMembership {
			return Response{}, fmt.Errorf("donor waited for membership before resuming frozen lot")
		}
		return net.Exchange(ctx, host, r)
	})

	net.setBefore(func(request Request) error {

		if request.Kind == "install-zone" && !stageFail.Swap(true) {
			if len(request.Seals) > 0 {
				mu.Lock()
				frozenFirst = request.Seals[0].Zone
				mu.Unlock()
			}
			return fmt.Errorf("injected stopped worker before install after freeze: %w", storage.ErrClosed)
		}
		if request.Kind == "install-zone" && checkResumed.Swap(false) {
			mu.Lock()
			want := frozenFirst
			mu.Unlock()
			for _, seal := range request.Seals {
				if seal.Zone == want {
					mu.Lock()
					firstResumedHasFrozen = true
					mu.Unlock()
					break
				}
			}
		}
		return nil
	})
	net.setAfter(func(request Request) error {
		if request.Kind == "install-zone" && !installFail.Swap(true) {
			z := request.ZoneTransfers[0].Zone
			mu.Lock()
			liveZone = z
			mu.Unlock()
			r := storage.Record{Collection: z.Collection, Location: z.Location, ID: "live-after-lost-ack", Metrics: []float64{7}}
			out, err := target.Handle(ctx, Request{Kind: "write", RequestID: "live-after-lost-ack", Record: &r})
			if err != nil || !out.AppliedLocally {
				return fmt.Errorf("receiver refused live write after durable install: %v", err)
			}
			return fmt.Errorf("injected stopped worker after lost install-zone ACK: %w", storage.ErrClosed)
		}
		return nil
	})

	// Attempt 1 stops after the combined fence/emit commit, before install.
	if err := target.Join(ctx); err == nil {
		t.Fatal("expected injected worker interruption before install")
	}
	// Attempt 2 must finish the already-frozen lot first, then hits lost install ACK.
	checkResumed.Store(true)
	if err := target.Join(ctx); err == nil {
		t.Fatal("expected injected lost install-zone ACK")
	}
	mu.Lock()
	resumed := firstResumedHasFrozen
	lz := liveZone
	mu.Unlock()
	if !resumed {
		t.Fatal("retry did not finish the already-frozen lot first")
	}
	// Attempt 3 replays the lost install proof and completes consolidation.
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := target.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, snap := range rows {
		if snap.Zone == lz {
			for _, r := range snap.Records {
				if r.ID == "live-after-lost-ack" {
					seen++
				}
			}
		}
	}
	if seen != 1 {
		t.Fatalf("live write after lost install-zone ACK seen %d times, want 1", seen)
	}
}

// A child zone born while its parent is being copied forces a rebuild and flips
// before its parent (ZONE-2) without stalling or skipping pre-freeze passes.
func TestGranularNewbornChildDuringParentCopyFlipsBeforeParent(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	ctx := context.Background()
	parent := storage.Record{Collection: "Demo000000000000", Location: "a", ID: "p"}
	child1 := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "c1"}
	pz, _ := tree.NewZoneKey(parent.Collection, parent.Location)
	target.VNode.ID = pz.ID
	for i, r := range []storage.Record{parent, child1} {
		if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("seed-%d", i), Record: &r}); err != nil {
			t.Fatal(err)
		}
	}
	var injected atomic.Bool
	var mu sync.Mutex
	var flipOrder []string
	net.setAfter(func(request Request) error {
		if request.Kind == "preload" && request.Transfer != nil {
			for _, snap := range request.Transfer.Snapshots {
				if snap.Zone.Location == "a" && !injected.Swap(true) {
					child2 := storage.Record{Collection: "Demo000000000000", Location: "ab", ID: "c2"}
					if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "newborn-child", Record: &child2}); err != nil {
						return err
					}
				}
			}
		}
		if request.Kind == "install-zone" {
			mu.Lock()
			for _, zt := range request.ZoneTransfers {
				flipOrder = append(flipOrder, zt.Zone.Location)
			}
			mu.Unlock()
		}
		return nil
	})
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if !injected.Load() {
		t.Fatal("newborn child was never injected during parent copy")
	}
	pos := map[string]int{}
	for i, loc := range flipOrder {
		pos[loc] = i
	}
	for _, child := range []string{"aa", "ab"} {
		if _, ok := pos[child]; !ok {
			t.Fatalf("child %s never installed: order=%v", child, flipOrder)
		}
		if pos[child] >= pos["a"] {
			t.Fatalf("parent flipped before child %s: order=%v", child, flipOrder)
		}
	}
}
