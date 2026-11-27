package autoscale_test

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// simClock is a deterministic virtual clock shared by all vnodes in a tick simulation.
type simClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSimClock() *simClock {
	return &simClock{now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
}

func (c *simClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *simClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type simVNode struct {
	id        tree.ID
	name      string
	root      bool
	weight    float64
	ctrl      *autoscale.Controller
	coldSince time.Time
}

type pendingJoin struct {
	landAt  time.Time
	donorID tree.ID
	target  tree.ID
	name    string
}

type simStats struct {
	maxVNodes    int
	minVNodes    int
	endVNodes    int
	splits       int
	folds        int
	oscillations int // split within 30s of a fold, or fold within 30s of a split
	maxPerVNode  float64
}

type clusterSim struct {
	clock       *simClock
	tick        time.Duration
	writesLimit float64
	writesHold  time.Duration
	cooldown    time.Duration
	joinLatency time.Duration
	coldHold    time.Duration
	coldFrac    float64 // combined fraction of WritesLimit below which COLD-2 may fold
	splitSkew   float64 // donor keeps splitSkew, joiner gets 1-splitSkew (0.5 = symmetric)

	vnodes      []*simVNode
	joins       []pendingJoin
	nextSeq     uint64
	lastSplitAt time.Time
	lastFoldAt  time.Time
	stats       simStats
}

func newClusterSim(writesLimit float64, joinLatency time.Duration) *clusterSim {
	s := &clusterSim{
		clock:       newSimClock(),
		tick:        2 * time.Second,
		writesLimit: writesLimit,
		writesHold:  10 * time.Second,
		cooldown:    15 * time.Second,
		joinLatency: joinLatency,
		coldHold:    30 * time.Second,
		coldFrac:    0.45,
		splitSkew:   0.50,
		stats:       simStats{minVNodes: math.MaxInt},
	}
	rootID := s.mintID(tree.ID{}, 0)
	s.vnodes = append(s.vnodes, s.newVNode(rootID, true, 1.0))
	return s
}

func (s *clusterSim) mintID(near tree.ID, keepBits int) tree.ID {
	s.nextSeq++
	var id tree.ID
	if keepBits > 0 {
		id = near
	}
	// Deterministic unique suffix in the low 4 bytes (32 bits), preserving the
	// high 8 bytes (64 bits) of the 96-bit tree.ID for SPAWN-1 / SCALE-1.
	seq := uint32(s.nextSeq)
	id[tree.Size-4] = byte(seq >> 24)
	id[tree.Size-3] = byte(seq >> 16)
	id[tree.Size-2] = byte(seq >> 8)
	id[tree.Size-1] = byte(seq)
	if keepBits == 0 {
		id[0] = byte(seq)
	}
	return id
}

func (s *clusterSim) newVNode(id tree.ID, root bool, weight float64) *simVNode {
	name := tree.BASE64.Encode(id[:])
	ctrl := autoscale.New(autoscale.Config{
		Enabled:      true,
		Role:         "sim",
		ItemsOnly:    true,
		WritesLimit:  s.writesLimit,
		WritesHold:   s.writesHold,
		Cooldown:     s.cooldown,
		MemRefusePct: 95,
		Now:          s.clock.Now,
	})
	// Deterministic placement target (no crypto/rand) derived from donor ID + split sequence.
	var splitSeq uint32
	ctrl.SetNearBuilder(func(string, int, string) []string {
		splitSeq++
		target := id
		target[4] ^= byte(splitSeq)
		target[5] ^= byte(splitSeq >> 8)
		target[6] ^= 0xA5
		return []string{tree.BASE64.Encode(target[:])}
	})
	return &simVNode{
		id:     id,
		name:   name,
		root:   root,
		weight: weight,
		ctrl:   ctrl,
	}
}

func (s *clusterSim) members() []tree.VNodeDescriptor {
	ids := make([]tree.VNodeDescriptor, len(s.vnodes))
	for i, v := range s.vnodes {
		ids[i] = tree.VNodeDescriptor{ID: v.id, Name: v.name}
	}
	return ids
}

func (s *clusterSim) step(totalWriteRate float64) {
	now := s.clock.Now()

	// 1. Land any joiners whose handoff completed at or before now.
	remaining := s.joins[:0]
	for _, j := range s.joins {
		if now.Before(j.landAt) {
			remaining = append(remaining, j)
			continue
		}
		var donor *simVNode
		for _, v := range s.vnodes {
			if v.id == j.donorID {
				donor = v
				break
			}
		}
		if donor == nil {
			// Donor already folded; drop orphan join.
			continue
		}
		joinWeight := donor.weight * (1.0 - s.splitSkew)
		donor.weight *= s.splitSkew
		s.vnodes = append(s.vnodes, s.newVNode(j.target, false, joinWeight))
	}
	s.joins = remaining

	// Normalize weights so totalWriteRate is conserved.
	var weightSum float64
	for _, v := range s.vnodes {
		weightSum += v.weight
	}

	// 2. Tick every vnode's autoscale controller deterministically.
	active := s.members()
	for _, v := range s.vnodes {
		rate := totalWriteRate * (v.weight / weightSum)
		if rate > s.stats.maxPerVNode {
			s.stats.maxPerVNode = rate
		}
		var reqCh chan autoscale.Request
		var triggered bool
		v.ctrl.Tick(autoscale.Input{
			SelfName:  v.name,
			WriteRate: rate,
			Members:   active,
		}, func(req autoscale.Request) error {
			triggered = true
			if reqCh != nil {
				reqCh <- req
			}
			return nil
		})
		// Wait deterministically for the async scaleUp goroutine (if spawned)
		// to finish before advancing virtual time — no time.Sleep.
		for v.ctrl.Snapshot()["up_in_flight"].(bool) {
			runtime.Gosched()
		}
		if triggered {
			s.stats.splits++
			if !s.lastFoldAt.IsZero() && now.Sub(s.lastFoldAt) < 30*time.Second {
				s.stats.oscillations++
			}
			s.lastSplitAt = now
			nearStr := v.ctrl.Snapshot()["last_prefer_near"].(string)
			raw, _ := tree.BASE64.Decode(nearStr)
			target := s.mintID(tree.FromSlice(raw), 64)
			s.joins = append(s.joins, pendingJoin{
				landAt:  now.Add(s.joinLatency),
				donorID: v.id,
				target:  target,
				name:    tree.BASE64.Encode(target[:]),
			})
		}
	}

	// 3. Model COLD-2 compaction when a non-root vnode stays cold and fits in a survivor.
	if len(s.vnodes) > 1 && len(s.joins) == 0 {
		for i := len(s.vnodes) - 1; i >= 1; i-- {
			v := s.vnodes[i]
			rate := totalWriteRate * (v.weight / weightSum)
			if rate < 0.25*s.writesLimit {
				if v.coldSince.IsZero() {
					v.coldSince = now
				}
			} else {
				v.coldSince = time.Time{}
			}
			if !v.coldSince.IsZero() && now.Sub(v.coldSince) >= s.coldHold {
				survivor := s.vnodes[i-1]
				combinedRate := totalWriteRate * ((v.weight + survivor.weight) / weightSum)
				if combinedRate <= s.coldFrac*s.writesLimit {
					survivor.weight += v.weight
					s.vnodes = append(s.vnodes[:i], s.vnodes[i+1:]...)
					s.stats.folds++
					if !s.lastSplitAt.IsZero() && now.Sub(s.lastSplitAt) < 30*time.Second {
						s.stats.oscillations++
					}
					s.lastFoldAt = now
					break // one fold per tick
				}
			}
		}
	}

	n := len(s.vnodes)
	if n > s.stats.maxVNodes {
		s.stats.maxVNodes = n
	}
	if n < s.stats.minVNodes {
		s.stats.minVNodes = n
	}
	s.stats.endVNodes = n
	s.clock.Advance(s.tick)
}

func (s *clusterSim) run(duration time.Duration, rateFn func(elapsed time.Duration) float64) {
	steps := int(duration / s.tick)
	for i := 0; i < steps; i++ {
		s.step(rateFn(time.Duration(i) * s.tick))
	}
}

func TestSim_StepRampCleanGrowth(t *testing.T) {
	t.Parallel()
	// Steps: 2k -> 4k -> 8k -> 16k writes/s over 16 minutes with WritesLimit=1000/s.
	sim := newClusterSim(1000, 20*time.Second)
	sim.run(16*time.Minute, func(elapsed time.Duration) float64 {
		switch {
		case elapsed < 4*time.Minute:
			return 2000
		case elapsed < 8*time.Minute:
			return 4000
		case elapsed < 12*time.Minute:
			return 8000
		default:
			return 16000
		}
	})
	// At 16k/s with 1000/s per vnode, ideal is >= 17 vnodes (power-of-two dichotomy -> 32).
	if sim.stats.endVNodes < 17 || sim.stats.endVNodes > 36 {
		t.Fatalf("endVNodes=%d want [17, 36] (splits=%d folds=%d)", sim.stats.endVNodes, sim.stats.splits, sim.stats.folds)
	}
	if sim.stats.folds != 0 || sim.stats.oscillations != 0 {
		t.Fatalf("unexpected folds=%d oscillations=%d during monotonic ramp", sim.stats.folds, sim.stats.oscillations)
	}
	t.Logf("steps 2k->16k/s: vnodes=%d splits=%d folds=%d oscillations=%d",
		sim.stats.endVNodes, sim.stats.splits, sim.stats.folds, sim.stats.oscillations)
}

func TestSim_ThresholdJitterZeroOscillation(t *testing.T) {
	t.Parallel()
	// Load oscillates +-20% around 92% of WritesLimit for 10 simulated minutes.
	sim := newClusterSim(1000, 20*time.Second)
	sim.run(10*time.Minute, func(elapsed time.Duration) float64 {
		// Period = 12s (fast jitter crossing 1000/s repeatedly while mean is 920/s).
		return 920 + 180*math.Sin(2*math.Pi*elapsed.Seconds()/12.0)
	})
	if sim.stats.splits != 0 {
		t.Fatalf("jitter around threshold caused %d splits (want 0)", sim.stats.splits)
	}
	if sim.stats.oscillations != 0 {
		t.Fatalf("oscillations=%d want 0", sim.stats.oscillations)
	}
}

func TestSim_ShortBurstFiltered(t *testing.T) {
	t.Parallel()
	// Repeated 6s bursts at 1.5x limit (< WritesHold=10s) separated by 24s of calm.
	sim := newClusterSim(1000, 20*time.Second)
	sim.run(6*time.Minute, func(elapsed time.Duration) float64 {
		if int(elapsed.Seconds())%30 < 6 {
			return 1500
		}
		return 400
	})
	if sim.stats.splits != 0 {
		t.Fatalf("short 6s bursts caused %d splits (want 0)", sim.stats.splits)
	}
}

func TestSim_HardSurgeImmediateReactionAndNoCascade(t *testing.T) {
	t.Parallel()
	// Sudden 3x surge with slow join (30s): must fire on tick 0, then hold at 1 pending (SCALE-1).
	sim := newClusterSim(1000, 30*time.Second)
	sim.step(3000)
	if sim.stats.splits != 1 {
		t.Fatalf("3x surge did not trigger on tick 0: splits=%d", sim.stats.splits)
	}
	// Next 28s while joiner is in flight: SCALE-1 must prevent duplicate spawns.
	for i := 0; i < 14; i++ {
		sim.step(3000)
	}
	if sim.stats.splits != 1 {
		t.Fatalf("duplicate splits while joiner in flight: splits=%d want 1", sim.stats.splits)
	}
	// Run 3 more minutes so the cluster settles cleanly at 4 vnodes (3000/4 = 750 < 1000).
	sim.run(3*time.Minute, func(time.Duration) float64 { return 3000 })
	if sim.stats.endVNodes != 4 {
		t.Fatalf("endVNodes=%d want 4 for 3000/s at 1000/s limit (splits=%d)", sim.stats.endVNodes, sim.stats.splits)
	}
	if sim.stats.oscillations != 0 {
		t.Fatalf("oscillations=%d want 0", sim.stats.oscillations)
	}
}

func TestSim_SpikeCalmReSpikeSymmetry(t *testing.T) {
	t.Parallel()
	// Full elasticity cycle: calm -> 6000/s spike -> calm (COLD-2 folds) -> 6000/s spike again.
	sim := newClusterSim(1000, 15*time.Second)
	var peak1, valley, peak2 int
	sim.run(14*time.Minute, func(elapsed time.Duration) float64 {
		switch {
		case elapsed < 1*time.Minute:
			return 300
		case elapsed < 5*time.Minute:
			return 6000
		case elapsed < 10*time.Minute:
			if peak1 == 0 {
				peak1 = len(sim.vnodes)
			}
			return 150
		default:
			if valley == 0 {
				valley = len(sim.vnodes)
			}
			return 6000
		}
	})
	peak2 = sim.stats.endVNodes

	if peak1 < 7 || peak1 > 10 {
		t.Fatalf("peak1=%d want [7, 10] for 6000/s", peak1)
	}
	if valley > 2 {
		t.Fatalf("valley=%d want <= 2 after 5m calm (folds=%d)", valley, sim.stats.folds)
	}
	if peak2 != peak1 {
		t.Fatalf("asymmetric re-growth: peak1=%d peak2=%d (splits=%d folds=%d)", peak1, peak2, sim.stats.splits, sim.stats.folds)
	}
	if sim.stats.oscillations != 0 {
		t.Fatalf("split/fold oscillations=%d want 0", sim.stats.oscillations)
	}
	t.Logf("elasticity cycle: peak1=%d -> valley=%d -> peak2=%d (splits=%d folds=%d osc=%d)",
		peak1, valley, peak2, sim.stats.splits, sim.stats.folds, sim.stats.oscillations)
}

func TestSim_SkewedSplitConvergesWithoutOscillation(t *testing.T) {
	t.Parallel()
	// Realistic 60/40 key-space skew on every dichotomy split at 5000/s.
	sim := newClusterSim(1000, 20*time.Second)
	sim.splitSkew = 0.60
	sim.run(6*time.Minute, func(time.Duration) float64 { return 5000 })

	var weightSum, maxRate float64
	for _, v := range sim.vnodes {
		weightSum += v.weight
	}
	for _, v := range sim.vnodes {
		if r := 5000 * (v.weight / weightSum); r > maxRate {
			maxRate = r
		}
	}
	if maxRate >= 1000 {
		t.Fatalf("skewed cluster did not shed load below limit: maxRate=%.1f vnodes=%d", maxRate, len(sim.vnodes))
	}
	if sim.stats.oscillations != 0 {
		t.Fatalf("oscillations=%d want 0", sim.stats.oscillations)
	}
	t.Logf("skewed 60/40 split at 5k/s: vnodes=%d maxRate=%.0f/s splits=%d",
		len(sim.vnodes), maxRate, sim.stats.splits)
}

func Example_simSummary() {
	sim := newClusterSim(1000, 20*time.Second)
	sim.run(4*time.Minute, func(time.Duration) float64 { return 4000 })
	fmt.Printf("vnodes=%d osc=%d\n", sim.stats.endVNodes, sim.stats.oscillations)
}
