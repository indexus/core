package mockup

import (
	"context"
	"fmt"
	"sync"

	"strings"
	"sync/atomic"
	"testing"
	"time"

	indexus "github.com/indexus/go-indexus-core/indexus"
)

// The root aggregate `@` summarises its depth-1 children, and a summary is only
// worth what its worst entry is worth. During a handoff the two can disagree:
// §7 accepts it — between the donor's Delegate and the receiver's Feed apply a
// zone answers short, and reads are eventually consistent. So the property to
// pin is not "they never disagree" but "every disagreement is transient": a
// mismatch that outlives the handoff is an aggregate error, and unlike a count
// it does not self-correct once summed into a parent.
//
// The residual risk of the parent-stub work sits on the other side of the same
// read: a parent whose child has no reachable owner holds rather than accept an
// approximate value from a cache. That is deliberate — a late stub is
// recoverable, a wrong one is not — but it only converges if some authority
// answers in practice. The test counts both, so the window has a number rather
// than an assumption.

func ownedZoneCount(t *testing.T, n *indexus.Node) int {
	t.Helper()
	own, err := n.Ownership()
	if err != nil {
		t.Fatalf("Ownership %s: %v", n.Name(), err)
	}
	zones := 0
	for _, locations := range own {
		zones += len(locations)
	}
	return zones
}

// childDisagreement reads every depth-1 child the root claims and returns the
// first whose owner disagrees with what the root published for it. A child that
// cannot be read is not a disagreement: that is the parent holding, and it is
// counted separately by the caller.
func childDisagreement(from *indexus.Node, collection string) (mismatch string, unreachable int) {
	root := indexus.BASE64.Root()
	_, rootSet, err := from.Get(collection, root, true, nil, false)
	if err != nil || rootSet == nil {
		return "", 0
	}

	for child, published := range rootSet.List() {
		// A parent set mixes item rows ("location:id") with its zone children.
		// Only the latter have an owner to compare against.
		if published == nil || strings.ContainsRune(child, ':') ||
			!indexus.IsDirectChild(indexus.BASE64, root, child) {
			continue
		}
		_, childSet, err := from.Get(collection, child, true, nil, false)
		if err != nil || childSet == nil {
			unreachable++
			continue
		}
		if owned := childSet.Abelian(); !published.IsEqual(owned) {
			return fmt.Sprintf("child %q: root says count=%d metrics=%v, owner says count=%d metrics=%v",
				child, published.Count(), published.Metrics(),
				owned.Count(), owned.Metrics()), unreachable
		}
	}
	return "", unreachable
}

// A second node joins a loaded mesh and takes zones over. Throughout the
// handoff the root must never publish a child entry the child's owner
// contradicts — the moment it does, the error is in the parent for good.
func TestRootAggregateNeverContradictsItsChildrenDuringHandoff(t *testing.T) {
	skipLong(t)
	ResetNetwork()

	const (
		items = 900
		// Low enough that the depth-1 zones actually split and become
		// individually owned — otherwise the root is the only zone there is,
		// and whether it moves at all is a coin flip on the node names.
		delegation = 10
		steadyFor  = 2 * time.Second
		stall      = 45 * time.Second
	)
	root := indexus.BASE64.Root()

	n1 := newConvergenceNode(t, delegation)
	collection, err := indexus.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}

	// Spread over depth-1 locations so the root carries many children and a
	// rebalance has something to split along.
	for i := 0; i < items; i++ {
		location := spreadLocation(i)
		if err := n1.New(&indexus.Item{
			Collection: collection,
			Location:   location,
			Id:         fmt.Sprintf("id-%d", i),
			Metrics:    []float64{1, 2, 3},
		}, root, nil); err != nil {
			t.Fatalf("New: %v", err)
		}
	}

	if mismatch, _ := childDisagreement(n1, collection); mismatch != "" {
		t.Fatalf("root already disagrees with its children before any handoff: %s", mismatch)
	}

	// The node exists but nothing has driven a Refresh yet, so the handoff has
	// not started: the watch is in place before the first zone moves.
	n2 := createConvergenceNode(t, "", delegation, false, n1)
	nodes := []*indexus.Node{n1, n2}

	// Hold the real transfer after the source's disk fence, before metadata
	// activation. The observation window must overlap an actual handoff.
	value, _ := durableMeshes.Load(network)
	runtime := value.(*durableNetwork)
	frozen, release := make(chan struct{}), make(chan struct{})
	var freezeOnce, releaseOnce sync.Once
	releaseFreeze := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFreeze()
	runtime.mu.Lock()
	runtime.after = func(operation string) {
		if operation == "freeze" {
			freezeOnce.Do(func() { close(frozen) })
			<-release
		}
	}
	runtime.mu.Unlock()

	// Sample throughout the handoff. Sampling only at the end would say nothing
	// about the window; sampling only during it would say nothing about whether
	// the window closes.
	var lastMismatch atomic.Value
	var samples, mismatches, holds atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, from := range nodes {
				bad, unreachable := childDisagreement(from, collection)
				if bad != "" {
					mismatches.Add(1)
					lastMismatch.Store(from.Name() + ": " + bad)
				}
				holds.Add(int64(unreachable))
				samples.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	var stopOnce sync.Once
	stopWatching := func() { stopOnce.Do(func() { close(stop) }); <-done }
	t.Cleanup(stopWatching)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	joined := make(chan error, 1)
	go func() { joined <- n2.JoinCausalCluster(ctx) }()
	select {
	case <-frozen:
	case <-ctx.Done():
		t.Fatal("transfer did not reach durable freeze")
	}
	before := samples.Load()
	deadline := time.Now().Add(2 * time.Minute)
	for samples.Load() < before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if samples.Load() < before+2 {
		t.Fatal("could not observe both edges during the frozen handoff")
	}
	if mismatches.Load() != 0 {
		t.Fatalf("frozen handoff contradicted aggregate: %v", lastMismatch.Load())
	}
	releaseFreeze()
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	converge(t, nodes, 6, 3, 40)
	awaitStability(t, nodes, items, steadyFor, stall)

	stopWatching()

	if got := samples.Load(); got < 2 {
		t.Fatalf("only %d samples taken during the handoff, the watch proved nothing", got)
	}
	// Without a zone actually crossing, the watch had nothing to catch.
	if zones := ownedZoneCount(t, n2); zones == 0 {
		t.Fatal("the joining node ended up owning nothing — no handoff was exercised")
	}
	t.Logf("handoff window: %d samples, %d disagreements, %d reads where no authority answered",
		samples.Load(), mismatches.Load(), holds.Load())

	// The window has to close. Sampling once could catch a lucky instant, so
	// hold the settled mesh to the invariant for a stretch.
	if total := totalCount(t, nodes); total != items {
		t.Fatalf("items after handoff: got %d want %d", total, items)
	}
	deadline = time.Now().Add(steadyFor)
	for time.Now().Before(deadline) {
		for _, from := range nodes {
			bad, unreachable := childDisagreement(from, collection)
			if bad != "" {
				t.Fatalf("a settled mesh still contradicts itself (last seen in flight: %v): %s",
					lastMismatch.Load(), bad)
			}
			if unreachable > 0 {
				t.Fatalf("a settled mesh has %d children no authority answers for, read from %s",
					unreachable, from.Name())
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertOwnershipDisjointAndComplete(t, nodes)
}
