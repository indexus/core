package indexus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/wire"
)

func TestReadModesKeepKnownResultsAvailableWithoutReopeningWrites(t *testing.T) {
	t.Parallel()
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = EncodeName(root.ID)
	f := newReadPhaseFixture(t, []VNodeDescriptor{root}, false, false)
	node := f.nodes[0]
	row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "one", Metrics: []float64{7}}
	if _, _, _, err := f.stores[0].ApplyRecord("seed", row); err != nil {
		t.Fatal(err)
	}
	result, err := node.ReadContext(context.Background(), "Demo000000000000", "a", ReadOptions{Mode: ReadFresh})
	if err != nil || result.Set == nil || result.Set.Abelian().Count() != 1 || result.Source != "inventory" {
		t.Fatalf("warm: %+v %v", result, err)
	}
	observed := result.ObservedAt
	// A reader is not allowed to mutate the private cached answer.
	result.Set.Put("aa:one", wire.NewAbelian(1, []float64{-999}))
	target := f.services[0].Self
	target.Storage, target.Host = "receiver-disk", "receiver"
	freezeWithConfirmedReceiver(t, f.stores[0], target)
	// A transfer in progress refuses no read: the cache answers ReadCached, and
	// ReadStaleIfError reads live from the frozen source.
	result, err = node.ReadContext(context.Background(), "Demo000000000000", "a", ReadOptions{Mode: ReadCached, MaxAge: time.Minute})
	if err != nil || result.Set == nil || result.Set.Abelian().Metric(0) != 7 || result.Source != "cache" || result.ObservedAt.After(observed) {
		t.Fatalf("%s: %+v %v", ReadCached, result, err)
	}
	result.Set.Put("aa:one", wire.NewAbelian(1, []float64{-999}))
	result, err = node.ReadContext(context.Background(), "Demo000000000000", "a", ReadOptions{Mode: ReadStaleIfError, MaxAge: time.Minute})
	if err != nil || result.Set == nil || result.Set.Abelian().Metric(0) != 7 {
		t.Fatalf("%s: %+v %v", ReadStaleIfError, result, err)
	}
	key := Key{Collection: "Demo000000000000", Location: "a"}
	node.mesh.reads.mu.Lock()
	old := node.mesh.reads.entries[key]
	old.at = time.Now().Add(-2 * time.Minute)
	node.mesh.reads.entries[key] = old
	node.mesh.reads.mu.Unlock()
	if out, err := node.ReadContext(context.Background(), "Demo000000000000", "a", ReadOptions{Mode: ReadStaleIfError, MaxAge: time.Minute}); err != nil || out.Source == "cache" {
		t.Fatalf("expired cache answered instead of a live read: %+v %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = node.ReadContext(ctx, "Demo000000000000", "a", ReadOptions{Mode: ReadCached, MaxAge: time.Hour}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation hidden: %v", err)
	}
}

func TestFreshReadIsServedDuringFrozenTransfer(t *testing.T) {
	t.Parallel()
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = EncodeName(root.ID)
	f := newReadPhaseFixture(t, []VNodeDescriptor{root}, false, false)
	row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "one"}
	if _, _, _, err := f.stores[0].ApplyRecord("seed", row); err != nil {
		t.Fatal(err)
	}
	// Inject a real on-disk fence, not a fabricated read error.
	target := f.services[0].Self
	target.Storage = "receiver-disk"
	target.Host = "receiver"
	freezeWithConfirmedReceiver(t, f.stores[0], target)
	// The source serves its frozen rows: a read during a transfer may be
	// stale, it is not refused and does not wait.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := f.nodes[0].ReadContext(ctx, "Demo000000000000", "a", ReadOptions{Mode: ReadFresh}); err != nil || out.Set == nil || out.Set.Count() != 1 {
		t.Fatalf("fresh read during a transfer: %+v %v", out, err)
	}
	if _, _, _, err := f.stores[0].ApplyRecord("forbidden", row); !errors.Is(err, storage.ErrFrozen) {
		t.Fatalf("read changed writer fence: %v", err)
	}
}

func TestReadCacheHasBoundedRetention(t *testing.T) {
	t.Parallel()
	var cache readCache
	for i := 0; i < 100; i++ {
		set := wire.NewSet()
		set.Put("record", wire.NewAbelian(i, nil))
		cache.put(Key{Collection: "Demo000000000000", Location: string(rune(i + 64))}, set, time.Now())
	}
	if len(cache.entries) > readCacheEntries || cache.bytes > readCacheBytes {
		t.Fatal("unbounded retained cache")
	}
	// Freshness is not extended by hits.
	key := Key{Collection: "Demo000000000000", Location: "bounded"}
	at := time.Now()
	set := wire.NewSet()
	cache.put(key, set, at)
	if got, stamp := cache.get(key, time.Second, at.Add(time.Second/2)); got == nil || !stamp.Equal(at) {
		t.Fatal("cache hit changed observation")
	}
	if got, _ := cache.get(key, time.Second, at.Add(2*time.Second)); got != nil {
		t.Fatal("expired hit refreshed itself")
	}
}

func TestCachedAndFreshReadsThroughRealColdCompaction(t *testing.T) {
	t.Parallel()
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = EncodeName(root.ID)
	child := VNodeDescriptor{ID: ID{127}}
	child.Name = EncodeName(child.ID)
	f := newReadPhaseFixture(t, []VNodeDescriptor{root, child}, false, false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const count = 128
	for i := 0; i < count; i++ {
		row := storage.Record{Collection: "0000000000000001", Location: EncodeName(ID{byte(i * 2)}), ID: fmt.Sprint(i), Metrics: []float64{1}}
		if _, err := f.services[0].Write(ctx, "seed:"+row.ID, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	for _, node := range f.nodes {
		out, err := node.ReadContext(ctx, "0000000000000001", "@", ReadOptions{Mode: ReadFresh})
		if err != nil || out.Set.Count() != count {
			t.Fatalf("warm: %v", err)
		}
	}
	checked := 0
	check := func(r mesh.Request) error {
		switch r.Kind {
		case "preload", "mirror", "stage-zone", "install-zone", "stage", "install":
		default:
			return nil
		}
		for _, node := range f.nodes {
			out, err := node.ReadContext(ctx, "0000000000000001", "@", ReadOptions{Mode: ReadCached, MaxAge: time.Minute})
			if err != nil || out.Set == nil || out.Set.Count() != count || out.Set.Abelian().Metric(0) != count || out.Source != "cache" {
				return fmt.Errorf("%s cached read unavailable/changed: %+v %v", r.Kind, out, err)
			}
			checked++
		}
		return nil
	}
	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.net.before = func(r mesh.Request) error {
		if err := check(r); err != nil {
			return err
		}
		if r.Kind == "install-zone" {
			once.Do(func() {
				close(paused)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
		return nil
	}
	f.net.after = check
	compactDone := make(chan error, 1)
	go func() { compactDone <- f.nodes[1].CompactCausal(ctx, CausalCompactionConfig{}) }()
	select {
	case <-paused:
	case err := <-compactDone:
		t.Fatalf("no installation pause: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	type answer struct {
		result ReadResult
		err    error
	}
	freshDone := make(chan answer, 1)
	go func() {
		out, err := f.nodes[0].ReadContext(ctx, "0000000000000001", "@", ReadOptions{Mode: ReadFresh})
		freshDone <- answer{out, err}
	}()
	// A fresh read is served before the installation (from the frozen source).
	select {
	case out := <-freshDone:
		if out.err != nil || out.result.Set == nil || out.result.Set.Count() != count {
			t.Fatalf("fresh read during the fold: %+v", out)
		}
		freshDone <- out
	case <-time.After(5 * time.Second):
		t.Fatal("fresh read waited for the installation")
	}
	close(release)
	if err := <-compactDone; err != nil {
		t.Fatal(err)
	}
	fresh := <-freshDone
	if fresh.err != nil || fresh.result.Source != "inventory" || fresh.result.Set == nil || fresh.result.Set.Count() != count {
		t.Fatalf("fresh read during the fold: %+v", fresh)
	}
	f.net.before = nil
	f.net.after = nil
	if checked == 0 {
		t.Fatal("no transfer boundary checked")
	}
	for _, node := range f.nodes {
		out, err := node.ReadContext(ctx, "0000000000000001", "@", ReadOptions{Mode: ReadFresh})
		if err != nil || out.Set.Count() != count || out.Source != "inventory" {
			t.Fatalf("post-retirement fresh: %+v %v", out, err)
		}
	}
	t.Logf("cached reads at real transfer boundaries: %d", checked)
}
