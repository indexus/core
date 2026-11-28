package mesh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Keep a real lane occupied in a separate process. All RPCs on the donor
// use Handle and the normal deduplicated handoff worker.
func TestUnfencedAdmissionFollowsItsRequesters(t *testing.T) {
	for _, scenario := range []string{"live", "last RPC cancelled", "RPC returns pending", "one follower remains", "spawn no longer useful"} {
		t.Run(scenario, func(t *testing.T) {
			_, nodes := meshFixture(t, 2)
			source, target := nodes[0], nodes[1]
			budget := hostBudgets(t, 1, 1)[0]
			budget.admission = &metrics.CopyAdmission{}
			source.SetCopyBudget(budget)
			hot := startCopyChild(t, budget.dir)
			hot.act(t, "try", "admitted")
			shrinkCopyWait(t, 15*time.Millisecond)
			prev := handoffWait
			handoffWait = time.Second
			if scenario == "RPC returns pending" {
				handoffWait = 200 * time.Millisecond
			}
			t.Cleanup(func() { handoffWait = prev })
			var wanted atomic.Bool
			wanted.Store(true)
			source.Wanted = wanted.Load
			state, err := source.Store.CausalState()
			if err != nil {
				t.Fatal(err)
			}
			req := Request{Kind: "handoff", Input: state.Genesis.Right().ID, Region: tree.Region{}, Target: target.Self, TargetVNode: target.VNode, Subdivide: true, Spawned: true}
			follow := func(ctx context.Context) <-chan error {
				done := make(chan error, 1)
				go func() { _, err := source.Handle(ctx, req); done <- err }()
				return done
			}
			first, cancel := context.WithCancel(t.Context())
			defer cancel()
			answer := follow(first)
			waitForCopyTest(t, func() bool {
				entries, err := os.ReadDir(filepath.Join(budget.dir, "source-queue"))
				return err == nil && len(entries) == 1
			})
			source.jobs.mu.Lock()
			job := source.jobs.jobs[handoffKey(req)]
			source.jobs.mu.Unlock()
			if job == nil {
				t.Fatal("queue lost its source job")
			}
			row := storage.Record{Collection: "Demo000000000000", Location: "a", ID: "during-queue"}
			if out, e := source.Write(t.Context(), row.ID, row); e != nil || !out.AppliedLocally {
				t.Fatalf("waiting donor write: applied %v, error %v", out.AppliedLocally, e)
			}
			// The ordinary per-call admission interval expires with the lane
			// still occupied; a live request must keep its unpinned ticket.
			time.Sleep(2 * copySlotWait)
			select {
			case err := <-answer:
				t.Fatalf("withdrew during live RPC: %v", err)
			default:
			}
			switch scenario {
			case "last RPC cancelled":
				cancel()
				if e := <-answer; !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
			case "RPC returns pending":
				if e := <-answer; !errors.Is(e, ErrHandoffPending) {
					t.Fatal(e)
				}
			case "one follower remains":
				other := follow(t.Context())
				waitForCopyTest(t, func() bool { return job.followers.Load() == 2 })
				cancel()
				if e := <-answer; !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
				answer = other
			case "spawn no longer useful":
				wanted.Store(false)
			}
			if scenario == "last RPC cancelled" || scenario == "RPC returns pending" {
				select {
				case <-job.done:
				case <-time.After(time.Second):
					t.Fatal("unfollowed proposal remained queued")
				}
				if !errors.Is(job.err, storage.ErrNotReady) {
					t.Fatal(job.err)
				}
				if a := budget.admission; a.Interrupted.Load() != 1 || a.RequesterLeft.Load() != 1 || a.Deferred.Load() != 0 || a.Granted.Load() != 0 {
					t.Fatal("withdrawn requester misclassified", a.Interrupted.Load(), a.RequesterLeft.Load(), a.Deferred.Load(), a.Granted.Load())
				}
				hot.act(t, "release", "ok")
				hot.act(t, "try", "admitted") // no ghost left in the queue
			} else {
				hot.act(t, "release", "ok")
				if e := <-answer; scenario == "spawn no longer useful" {
					if !errors.Is(e, ErrNotNeeded) {
						t.Fatal(e)
					}
				} else {
					if e != nil {
						t.Fatal(e)
					}
					zone, e := tree.NewZoneKey(row.Collection, row.Location)
					if e != nil {
						t.Fatal(e)
					}
					snapshot, e := target.Store.Snapshot(zone)
					if e != nil || len(snapshot.Records) != 1 || snapshot.Records[0].ID != row.ID {
						t.Fatalf("receiver lost acknowledged record %q: %+v, %v", row.ID, snapshot.Records, e)
					}
					row.ID = "owner"
					if _, _, applied, e := target.Store.ApplyRecord("target", row); !applied || e != nil {
						t.Fatalf("target missing right %t %v", applied, e)
					}
					if _, _, _, e := source.Store.ApplyRecord("source", row); !errors.Is(e, storage.ErrNotOwner) {
						t.Fatalf("source still writable: %v", e)
					}
					return
				}
			}
			after, err := source.Store.CausalState()
			if err != nil || len(after.Transfers) != 0 {
				t.Fatalf("unfollowed or unneeded offer mutated authority %v %v", after.Transfers, err)
			}
			if _, ok := after.Permissions.Find(req.Input); !ok {
				t.Fatal("source right no longer installed")
			}
		})
	}
}

func waitForCopyTest(t *testing.T, condition func() bool) {
	t.Helper()
	end := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(end) {
			t.Fatal("copy condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}
