package tree

import (
	"math/rand/v2"
	"testing"
	"time"
)

func cellDepths(active []VNodeDescriptor) (minDepth, maxDepth int) {
	minDepth = 1 << 30
	for _, v := range active {
		d := 0
		for _, o := range active {
			if o.ID != v.ID {
				d = max(d, int(CommonPrefixLen(v.ID, o.ID))+1)
			}
		}
		minDepth, maxDepth = min(minDepth, d), max(maxDepth, d)
	}
	return
}

// JOIN-1: sequential joins through JoinHint keep cell sizes within a factor 2
// (depths differ by at most one), whatever the count; random ids do not.
func TestJOIN1_HintedJoinsKeepCellsWithinFactorTwo(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	for _, n := range []int{2, 3, 7, 16, 24, 64, 100} {
		hinted := []VNodeDescriptor{{ID: JoinHint(nil, rng)}}
		random := []VNodeDescriptor{{ID: JoinHint(nil, rng)}}
		for len(hinted) < n {
			hinted = append(hinted, VNodeDescriptor{ID: JoinHint(hinted, rng)})
			random = append(random, VNodeDescriptor{ID: JoinHint(nil, rng)})
		}
		lo, hi := cellDepths(hinted)
		if hi-lo > 1 {
			t.Fatalf("n=%d: hinted cell depths %d..%d differ by more than one", n, lo, hi)
		}
		rlo, rhi := cellDepths(random)
		t.Logf("n=%3d hinted largest/smallest cell = 2^%d, random = 2^%d", n, hi-lo, rhi-rlo)
	}
}

// REM-120: the bootstrap that answers hints sees joins only after a gossip
// round. Joins faster than that, hinted from the stale view alone, halve one
// cell twice and leave a double cell; remembering the hints given keeps the
// layout within a factor 2. The view here lags by several joins.
func TestJOIN1_StaleViewHintsKeepCellsWithinFactorTwo(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		rng := rand.New(rand.NewPCG(seed, 120))
		var hints RecentHints
		now := time.Unix(0, 0)
		joined := []VNodeDescriptor{{ID: JoinHint(nil, rng)}}
		view := append([]VNodeDescriptor(nil), joined...)
		for len(joined) < 64 {
			id := hints.Next(view, now, 2*time.Minute, rng)
			joined = append(joined, VNodeDescriptor{ID: id})
			now = now.Add(3 * time.Second) // one join every 3 s
			if len(joined)%4 == 0 {        // gossip: the view catches up every 12 s
				view = append(view[:0], joined...)
			}
		}
		if lo, hi := cellDepths(joined); hi-lo > 1 {
			t.Fatalf("seed %d: cell depths %d..%d: a cell is more than twice another", seed, lo, hi)
		}
	}
}

// Hints whose joiner never came expire; a joiner that appears in the view is
// no longer counted twice.
func TestJOIN1_RecentHintsForgetJoinedAndExpired(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	var hints RecentHints
	now := time.Unix(0, 0)
	active := []VNodeDescriptor{{ID: JoinHint(nil, rng)}}
	a := hints.Next(active, now, time.Minute, rng)
	hints.Next(active, now, time.Minute, rng)
	if len(hints.given) != 2 {
		t.Fatalf("%d hints remembered, want 2", len(hints.given))
	}
	hints.Next(append(active, VNodeDescriptor{ID: a}), now.Add(time.Second), time.Minute, rng)
	if len(hints.given) != 2 {
		t.Fatalf("after a joined: %d hints remembered, want 2", len(hints.given))
	}
	hints.Next(active, now.Add(2*time.Minute), time.Minute, rng)
	if len(hints.given) != 1 {
		t.Fatalf("after expiry: %d hints remembered, want 1", len(hints.given))
	}
}
