package summary

import (
	"math"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/wire"
)

func TestAcceptChildIgnoresDuplicateAndOlderRevision(t *testing.T) {
	zone := map[string]summaryContribution{}
	first := summaryContribution{revision: 2, generation: 1, holder: causal.Endpoint{Owner: causal.Owner{VNode: "A"}}, value: wire.NewAbelian(3, []float64{3})}
	if !acceptChild(zone, "aa", first) {
		t.Fatal("first contribution must apply")
	}
	if acceptChild(zone, "aa", first) {
		t.Fatal("duplicate revision must be ignored")
	}
	older := first
	older.revision = 1
	older.value = wire.NewAbelian(9, []float64{9})
	if acceptChild(zone, "aa", older) {
		t.Fatal("older revision must not resurrect a total")
	}
	if mustSummaryChild(t, zone, "aa").value.Count() != 3 {
		t.Fatalf("count=%d", mustSummaryChild(t, zone, "aa").value.Count())
	}
}

func TestAcceptChildNewerRevisionAndGenerationWin(t *testing.T) {
	zone := map[string]summaryContribution{}
	acceptChild(zone, "aa", summaryContribution{revision: 1, generation: 1, holder: causal.Endpoint{Owner: causal.Owner{VNode: "A"}}, value: wire.NewAbelian(1, []float64{1})})
	newer := summaryContribution{revision: 2, generation: 1, holder: causal.Endpoint{Owner: causal.Owner{VNode: "A"}}, value: wire.NewAbelian(4, []float64{4})}
	if !acceptChild(zone, "aa", newer) {
		t.Fatal("newer revision must replace")
	}
	staleAuthority := summaryContribution{revision: 9, generation: 0, holder: causal.Endpoint{Owner: causal.Owner{VNode: "A"}}, value: wire.NewAbelian(99, []float64{99})}
	if acceptChild(zone, "aa", staleAuthority) {
		t.Fatal("older generation from the same holder must not resurrect totals")
	}
	// A different holder is only presented after the parent verified it is the
	// child's current owner; its revision counter restarts, so it replaces.
	handoff := summaryContribution{revision: 1, generation: 1, holder: causal.Endpoint{Owner: causal.Owner{VNode: "B"}}, value: wire.NewAbelian(5, []float64{5})}
	if !acceptChild(zone, "aa", handoff) {
		t.Fatal("new holder must replace regardless of revision")
	}
	if mustSummaryChild(t, zone, "aa").value.Count() != 5 || mustSummaryChild(t, zone, "aa").holder.VNode != "B" {
		t.Fatalf("handoff=%v", mustSummaryChild(t, zone, "aa"))
	}
	if acceptChild(zone, "aa", summaryContribution{revision: 1, generation: 1, holder: causal.Endpoint{Owner: causal.Owner{VNode: "B"}}, value: wire.NewAbelian(7, nil)}) {
		t.Fatal("duplicate from the new holder must be ignored")
	}
}

func TestLocalWriteMarksInsertReplaceAndDeleteButNotAbsentTombstones(t *testing.T) {
	const collection, location = "Demo000000000000", "aa"
	leaf, err := tree.NewZoneKey(collection, location)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := tree.NewZoneKey(collection, tree.BASE64.Parent(location))
	if err != nil {
		t.Fatal(err)
	}
	view, err := tree.NewViewFrom(1, []tree.VNodeDescriptor{
		{Name: "mine", ID: leaf.ID}, {Name: "remote", ID: parent.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	cell, err := tree.Cell(view, "mine")
	if err != nil {
		t.Fatal(err)
	}
	live, gone := storage.Record{Metrics: []float64{3}}, storage.Record{Deleted: true}
	for _, tc := range []struct {
		name     string
		deleted  bool
		previous *storage.Record
		marked   bool
	}{
		{"insert", false, nil, true},
		{"replace", false, &live, true},
		{"recreate", false, &gone, true},
		{"delete", true, &live, true},
		{"delete missing", true, nil, false},
		{"repeat delete", true, &gone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Keep a flush logically in progress: this notification-only fixture
			// has no backing service. Retire its real timer after the assertion.
			mark := &Tree{name: "mine", pushing: true}
			t.Cleanup(func() {
				mark.dirtyMu.Lock()
				if mark.dirtyTimer != nil {
					mark.dirtyTimer.Stop()
					mark.dirtyTimer = nil
				}
				mark.dirtyMu.Unlock()
			})
			mark.SetView(view, true, false)
			boundary, ok := mark.boundaryOf(collection, location)
			if !ok {
				t.Fatal("fixture has no remote-parent boundary")
			}
			mark.OnLocalWrite(storage.Record{Collection: collection, Location: location, Deleted: tc.deleted}, tc.previous, cell)
			mark.dirtyMu.Lock()
			_, got := mark.dirty[boundary]
			mark.dirtyMu.Unlock()
			if got != tc.marked {
				t.Fatalf("marked=%v want %v", got, tc.marked)
			}
		})
	}
}

func TestCoalesceDecisionIdleSendsImmediately(t *testing.T) {
	now := time.Unix(10, 0)
	send, wait := coalesceDecision(time.Time{}, now, time.Second)
	if !send || wait != 0 {
		t.Fatalf("idle send=%v wait=%s", send, wait)
	}
	send, wait = coalesceDecision(now.Add(-time.Second), now, time.Second)
	if !send || wait != 0 {
		t.Fatalf("window elapsed send=%v wait=%s", send, wait)
	}
	send, wait = coalesceDecision(now.Add(-400*time.Millisecond), now, time.Second)
	if send || wait != 600*time.Millisecond {
		t.Fatalf("coalesce send=%v wait=%s", send, wait)
	}
}

func TestCoalesceWriteRampMatchesModel(t *testing.T) {
	const step, window, writesEvery, writesFor = 10 * time.Millisecond, time.Second, 10 * time.Millisecond, 3500 * time.Millisecond
	var lastAttempt time.Time
	current, published, calls := 0, 0, 0
	start := time.Unix(0, 0)
	for elapsed := time.Duration(0); elapsed <= writesFor+window; elapsed += step {
		now := start.Add(elapsed)
		if elapsed < writesFor && elapsed%writesEvery == 0 {
			current++
		}
		if current == published {
			continue
		}
		send, _ := coalesceDecision(lastAttempt, now, window)
		if send {
			published = current
			lastAttempt = now
			calls++
		}
	}
	if published != 350 {
		t.Fatalf("published=%d want 350", published)
	}
	if calls != 5 {
		t.Fatalf("calls=%d want 5", calls)
	}
}

func TestSummaryEpochOrdersNewIncarnationBeforeVolatileCounters(t *testing.T) {
	zone := map[string]summaryContribution{}
	holder := causal.Endpoint{Host: "same-host", Owner: causal.Owner{VNode: "child", Storage: "same-durable-disk"}}
	makeVersion := func(epoch, generation, revision uint64, count int) summaryContribution {
		return summaryContribution{epoch: epoch, generation: generation, revision: revision, holder: holder, value: wire.NewAbelian(count, nil)}
	}
	if !acceptChild(zone, "a", makeVersion(1, 100, 100, 10)) {
		t.Fatal("first version rejected")
	}
	if !acceptChild(zone, "a", makeVersion(2, 0, 1, 11)) {
		t.Fatal("new incarnation with lower volatile counters rejected")
	}
	if acceptChild(zone, "a", makeVersion(1, math.MaxUint64, math.MaxUint64, 99)) {
		t.Fatal("older incarnation resurrected a value")
	}
	if acceptChild(zone, "a", makeVersion(2, 0, 1, 99)) {
		t.Fatal("duplicate incarnation/version replaced a value")
	}
	if !acceptChild(zone, "a", makeVersion(2, 1, 1, 12)) {
		t.Fatal("new generation rejected")
	}
	if acceptChild(zone, "a", makeVersion(2, 0, math.MaxUint64, 99)) {
		t.Fatal("older generation resurrected a value")
	}
	if got := mustSummaryChild(t, zone, "a").value.Count(); got != 12 {
		t.Fatalf("count=%d", got)
	}
}

func TestOlderInventoryCompletionCannotClearANewerRetry(t *testing.T) {
	m := &Tree{foldAttempt: 2, LastFold: time.Now()}
	m.finishFold(2, [32]byte{}, 2, false)
	m.finishFold(1, [32]byte{1}, 1, true)
	if !m.LastFold.IsZero() {
		t.Fatal("older fold cleared a newer incomplete repair")
	}
	m.foldAttempt = 3
	m.finishFold(3, [32]byte{2}, 3, true)
	if m.LastFold.IsZero() || m.lastFoldDigest != [32]byte{2} {
		t.Fatal("latest completed repair was not published")
	}
}
