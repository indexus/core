package mesh

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func streamFixture(t testing.TB, zones int) (*network, *Service, *Service) {
	t.Helper()
	net, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	for i := range zones {
		record := storage.Record{Collection: fmt.Sprintf("collection-%05d", i), Location: "aa", ID: "initial"}
		if _, err := source.Handle(context.Background(), Request{Kind: "write", RequestID: fmt.Sprintf("initial-%04d", i), Record: &record}); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := tree.NewZoneKey("collection-00000", "aa")
	target.VNode.ID = first.ID
	return net, source, target
}

func preloadRows(request Request) int {
	rows := 0
	for _, snap := range request.Transfer.Snapshots {
		rows += len(snap.Records)
	}
	return rows
}

// HANDOFF-STREAM: a region moves as several bounded preload pages, never one
// request carrying the whole region.
func TestHandoffStreamsPreloadInBoundedPages(t *testing.T) {
	defer func(old int) { preloadPageRecords = old }(preloadPageRecords)
	preloadPageRecords = 50
	net, _, target := streamFixture(t, 300)
	var mu sync.Mutex
	pages, largest := 0, 0
	net.setBefore(func(request Request) error {
		if request.Kind == "preload" {
			mu.Lock()
			pages++
			largest = max(largest, preloadRows(request))
			mu.Unlock()
		}
		return nil
	})
	if err := target.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if largest > preloadPageRecords {
		t.Fatalf("a preload carried %d rows, page is %d", largest, preloadPageRecords)
	}
	if pages < 300/preloadPageRecords {
		t.Fatalf("only %d preload pages for 300 zones", pages)
	}
}

// HANDOFF-STREAM: a later convergence round resends only the zones the
// receiver lacks; zones it holds advance through the mutation mirror.
func TestHandoffRoundsResendOnlyMissingZones(t *testing.T) {
	t.Parallel()
	net, source, target := streamFixture(t, 520)
	ctx := context.Background()
	// Each early preload lands a backlog above the freeze floor in existing
	// zones, forcing more rounds.
	var rounds atomic.Int32
	var copied atomic.Int64
	net.setAfter(func(request Request) error {
		if request.Kind != "preload" {
			return nil
		}
		copied.Add(int64(len(request.Transfer.Snapshots)))
		round := rounds.Add(1)
		if round > 2 {
			return nil
		}
		for i := range 300 {
			record := storage.Record{Collection: fmt.Sprintf("collection-%05d", i), Location: "aa", ID: fmt.Sprintf("round-%d", round)}
			if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("round-%d-%04d", round, i), Record: &record}); err != nil {
				return err
			}
		}
		return nil
	})
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if rounds.Load() < 3 {
		t.Fatalf("fixture exercised %d preloads", rounds.Load())
	}
	// One full copy plus a one-zone stand-in per later preload.
	if limit := int64(520) + int64(rounds.Load()); copied.Load() > limit {
		t.Fatalf("rounds recopied zones: %d snapshots sent, at most %d needed", copied.Load(), limit)
	}
}

// HANDOFF-STREAM: a handoff outliving one request runs on as one background
// job. Retries see it pending and join it instead of starting another copy.
func TestHandoffRetriesJoinTheRunningJob(t *testing.T) {
	defer func(old time.Duration) { handoffWait = old }(handoffWait)
	handoffWait = 50 * time.Millisecond
	net, _, target := streamFixture(t, 4)
	release := make(chan struct{})
	var inFlight, overlapped, asks atomic.Int32
	var once sync.Once
	net.setBefore(func(request Request) error {
		switch request.Kind {
		case "handoff":
			asks.Add(1)
		case "preload":
			if inFlight.Add(1) > 1 {
				overlapped.Store(1)
			}
			once.Do(func() { <-release })
			inFlight.Add(-1)
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- target.Join(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for asks.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if asks.Load() < 3 {
		t.Fatalf("the joiner never retried a pending handoff: %d asks", asks.Load())
	}
	if overlapped.Load() != 0 {
		t.Fatal("a retry started a second copy beside the running job")
	}
}

// KNOW-LAZY: a write the target applies itself carries no knowledge back; a
// write that had to be relayed returns the bundle so the writer can reroute.
func TestDirectWritesCarryNoKnowledge(t *testing.T) {
	t.Parallel()
	_, source, target := streamFixture(t, 64)
	ctx := context.Background()
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	direct, relayed := 0, 0
	for i := range 64 {
		record := storage.Record{Collection: fmt.Sprintf("collection-%05d", i), Location: "aa", ID: "again"}
		for _, node := range []*Service{source, target} {
			out, err := node.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("again-%s-%d", node.VNode.Name, i), Record: &record})
			if err != nil {
				t.Fatal(err)
			}
			if out.AppliedBy == node.Self {
				direct++
				if out.Membership != nil {
					t.Fatal("a direct write returned knowledge")
				}
				continue
			}
			relayed++
			if out.Membership != nil {
				t.Fatal("a relayed write carried membership")
			}
		}
	}
	if direct == 0 || relayed == 0 {
		t.Fatalf("fixture did not exercise both paths: direct=%d relayed=%d", direct, relayed)
	}
}
