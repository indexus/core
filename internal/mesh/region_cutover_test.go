package mesh

import (
	"context"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/testkit"
	"github.com/indexus/go-indexus-core/internal/tree"
	"sync"
	"testing"
	"time"
)

func TestNewZonesWaitForRegionalInstallWhileInstalledLotsKeepWriting(t *testing.T) {
	for _, pause := range []string{"stage", "install"} {
		t.Run(pause, func(t *testing.T) {
			net, source, target := streamFixture(t, 1)
			ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(10*time.Second))
			defer cancel()
			reached, release := make(chan struct{}), make(chan struct{})
			var reachedOnce, releaseOnce sync.Once
			net.setBefore(func(r Request) error {
				if r.Kind != pause {
					return nil
				}
				reachedOnce.Do(func() { close(reached) })
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			state, err := source.Store.CausalState()
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				done <- source.handoff(ctx, Request{Input: state.Genesis.Right().ID, Target: target.Self, Region: tree.Region{}})
				close(done)
			}()
			defer func() {
				releaseOnce.Do(func() { close(release) })
				select {
				case <-done:
				case <-ctx.Done():
					t.Error("handoff did not stop")
				}
			}()
			select {
			case <-reached:
			case err := <-done:
				t.Fatalf("handoff ended before native pause: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// These addresses did not exist during the warm copy. The receiver has
			// no point permission for them until the regional right is installed.
			writes := make(chan error, 2)
			for i, node := range []*Service{source, target} {
				go func() {
					row := storage.Record{Collection: fmt.Sprintf("new-zone-%07d", i), Location: "ab", ID: "fresh"}
					out, e := node.Write(ctx, fmt.Sprint("fresh-", i), row)
					if e == nil && out.AppliedBy.Owner != target.Self.Owner {
						e = fmt.Errorf("fresh write applied by wrong disk: %v", out.AppliedBy)
					}
					writes <- e
				}()
			}
			select {
			case err := <-writes:
				t.Fatalf("fresh address did not wait for native install: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			// A known lot is already writable and must not wait behind that scope.
			hot := storage.Record{Collection: "collection-00000", Location: "aa", ID: "ACK-during-native-close"}
			hotDone := make(chan error, 1)
			go func() { _, e := target.Write(ctx, hot.ID, hot); hotDone <- e }()
			select {
			case err := <-hotDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(testkit.RaceScale(time.Second)):
				t.Fatal("installed lot waited for regional close")
			}
			releaseOnce.Do(func() { close(release) })
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := <-writes; err != nil {
					t.Fatalf("healthy regional cutover refused a write: %v", err)
				}
			}
			rows, err := source.Query(ctx)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]int{}
			count := 0
			for _, snap := range rows {
				for _, row := range snap.Records {
					count++
					seen[row.Collection+"/"+row.ID]++
				}
			}
			if count != 4 {
				t.Fatalf("regional cutover lost ACKs or duplicated rows: %d %v", count, seen)
			}
		})
	}
}

func TestCancelledRegionalCutoverReleasesWaitersAndCapacity(t *testing.T) {
	net, source, target := streamFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(5*time.Second))
	defer cancel()
	reached := make(chan struct{})
	var once sync.Once
	net.setBefore(func(r Request) error {
		if r.Kind != "stage" {
			return nil
		}
		once.Do(func() { close(reached) })
		<-ctx.Done()
		return ctx.Err()
	})
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- source.handoff(ctx, Request{Input: state.Genesis.Right().ID, Target: target.Self, Region: tree.Region{}})
	}()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	source.WriteRate = 1
	writeCtx, stopWrite := context.WithCancel(ctx)
	defer stopWrite()
	writeDone := make(chan error, 1)
	go func() {
		_, e := source.Write(writeCtx, "cancelled-new-zone", storage.Record{Collection: "new-zone-0000000", Location: "ab", ID: "cancelled"})
		writeDone <- e
	}()
	select {
	case err := <-writeDone:
		t.Fatalf("write did not wait on regional fence: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	stopWrite()
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("cancelled write was acquitted")
		}
	case <-time.After(testkit.RaceScale(time.Second)):
		t.Fatal("cancelled waiter did not return")
	}
	source.writeCap.mu.Lock()
	tokens := source.writeCap.tokens
	source.writeCap.mu.Unlock()
	if tokens != 1 {
		t.Fatalf("cancelled write pinned admission token: %v", tokens)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled handoff succeeded")
		}
	case <-time.After(testkit.RaceScale(time.Second)):
		t.Fatal("cancelled handoff did not release scope")
	}
	source.cutoverMu.Lock()
	remaining := len(source.regionCutovers) + len(source.cutovers)
	source.cutoverMu.Unlock()
	if remaining != 0 {
		t.Fatalf("cancelled handoff leaked %d cutover barriers", remaining)
	}
}
