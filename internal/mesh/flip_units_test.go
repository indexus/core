package mesh

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func unitZone(t *testing.T, collection, location string) tree.ZoneKey {
	t.Helper()
	z, err := tree.NewZoneKey(collection, location)
	if err != nil {
		t.Fatal(err)
	}
	return z
}

// FLIP-UNIT: deeper zones come in earlier levels, so a parent flips after
// every child (ZONE-2); a unit holds at most size zones of one depth.
func TestFlipUnitsPutChildrenBeforeParentsInBoundedUnits(t *testing.T) {
	const a, b = "collection-0000a", "collection-0000b"
	var zones []tree.ZoneKey
	for _, location := range []string{"A", "AB", "ABC", "ABD", "ABE", "AC"} {
		zones = append(zones, unitZone(t, a, location))
	}
	zones = append(zones, unitZone(t, b, "ABC"), unitZone(t, a, ""))
	levels := flipUnits(zones, nil, 2)
	seen := map[tree.Key]int{}
	total := 0
	for level, units := range levels {
		for _, unit := range units {
			if len(unit) == 0 || len(unit) > 2 {
				t.Fatalf("a unit of %d zones, at most 2 asked", len(unit))
			}
			for _, z := range unit {
				if zoneDepth(z) != zoneDepth(unit[0]) {
					t.Fatalf("unit mixes depths: %+v", unit)
				}
				seen[z.Key()] = level
				total++
			}
		}
	}
	if total != len(zones) || len(seen) != len(zones) {
		t.Fatalf("%d zones planned (%d distinct), %d given", total, len(seen), len(zones))
	}
	for _, child := range zones {
		for _, parent := range zones {
			if parent.IsAncestorOf(child) && seen[parent.Key()] <= seen[child.Key()] {
				t.Fatalf("parent %q flips at level %d, its child %q at level %d", parent.Location, seen[parent.Key()], child.Location, seen[child.Key()])
			}
		}
	}
}

// FLIP-UNIT: within a level the most written unit goes first.
func TestFlipUnitsPutTheMostWrittenUnitFirst(t *testing.T) {
	const c = "collection-0000a"
	var zones []tree.ZoneKey
	for _, location := range []string{"AA", "AB", "BA", "BB", "CA", "CB"} {
		zones = append(zones, unitZone(t, c, location))
	}
	heat := func(k tree.Key) int64 {
		if k.Location == "CA" {
			return 9
		}
		if k.Location == "BB" {
			return 4
		}
		return 0
	}
	levels := flipUnits(zones, heat, 2)
	if len(levels) != 1 || len(levels[0]) != 3 {
		t.Fatalf("levels: %+v", levels)
	}
	if got := levels[0][0][0].Location + levels[0][1][0].Location + levels[0][2][0].Location; got != "CABAAA" {
		t.Fatalf("units start at %s, want the hottest first (CA, BA, AA)", got)
	}
}

// FLIP-UNIT: a join flips in small units, several in flight, and no request
// of the final phase carries more than one unit's zones.
func TestJoinFlipsInBoundedUnitsSideBySide(t *testing.T) {
	t.Parallel()
	net, source, target := streamFixture(t, 400)
	source.FlipUnitZones, source.FlipUnitsInFlight = 16, 4
	var mu sync.Mutex
	largest, open, widest, units := 0, 0, 0, 0
	net.setBefore(func(request Request) error {
		mu.Lock()
		defer mu.Unlock()
		switch request.Kind {
		case "install-zone":
			n := max(len(request.Seals), len(request.ZoneTransfers))
			if request.Transfer != nil {
				n = max(n, len(request.Transfer.Snapshots))
			}
			largest = max(largest, n)
			open++
			widest = max(widest, open)
			units++
		}
		return nil
	})
	net.setAfter(func(request Request) error {
		if request.Kind == "install-zone" {
			mu.Lock()
			open--
			mu.Unlock()
		}
		return nil
	})
	ctx := context.Background()
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if largest > 16 {
		t.Fatalf("a request of the final phase carried %d zones, a unit is 16", largest)
	}
	if units < 400/16 {
		t.Fatalf("%d units for 400 zones of 16", units)
	}
	if widest < 2 || widest > 4 {
		t.Fatalf("%d units in flight at most, want 2 to 4", widest)
	}
	// Every zone has one writer, the target, and kept its record.
	for i := range 400 {
		collection := fmt.Sprintf("collection-%05d", i)
		record := storage.Record{Collection: collection, Location: "aa", ID: "after"}
		if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: fmt.Sprintf("after-%04d", i), Record: &record}); err != nil {
			t.Fatalf("write after the join, zone %d: %v", i, err)
		}
	}
	snaps, err := target.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, snap := range snaps {
		rows += len(snap.Records)
	}
	if rows != 800 {
		t.Fatalf("%d rows after the join, want 800 (400 before, 400 after)", rows)
	}
}

func TestAdaptiveUnitSizerAIMDAndDeadband(t *testing.T) {
	t.Parallel()
	s := newAdaptiveUnit(0)
	if got := s.Current(); got != 16 {
		t.Fatalf("initial size = %d, want 16", got)
	}
	// Fast full cutovers (< 20 ms) double smoothly up to flipUnitMaxZones (2048).
	for _, want := range []int{32, 64, 128, 256, 512, 1024, 2048, 2048} {
		s.Observe(s.Current(), 10*time.Millisecond)
		if got := s.Current(); got != want {
			t.Fatalf("after fast cutover: got %d, want %d", got, want)
		}
	}
	// Inside the hysteresis deadband [20 ms, 80 ms], size stays steady (zero oscillation).
	for _, held := range []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond} {
		s.Observe(s.Current(), held)
		if got := s.Current(); got != 2048 {
			t.Fatalf("deadband (%v) changed size to %d, want 2048", held, got)
		}
	}
	// Slow cutovers (> 80 ms) halve smoothly down to flipUnitMinZones (4).
	// The hold follows the size (25 ms a zone), so every halving pays.
	for _, want := range []int{1024, 512, 256, 128, 64, 32, 16, 8, 4, 4} {
		s.Observe(s.Current(), time.Duration(s.Current())*25*time.Millisecond)
		if got := s.Current(); got != want {
			t.Fatalf("after slow cutover: got %d, want %d", got, want)
		}
	}
	// A truncated fast tail lot (< current size) does not inflate the target size.
	s.Observe(2, 5*time.Millisecond)
	if got := s.Current(); got != 4 {
		t.Fatalf("partial fast lot inflated size to %d, want 4", got)
	}
	// Explicit override remains fixed regardless of observed latency.
	fixed := newAdaptiveUnit(48)
	fixed.Observe(48, 5*time.Millisecond)
	fixed.Observe(48, 5*time.Second)
	if got := fixed.Current(); got != 48 {
		t.Fatalf("fixed override changed to %d, want 48", got)
	}
}

// TestAdaptiveUnitTickSimulation drives adaptiveUnit through deterministic
// disk-latency regimes to verify fast convergence and zero oscillation.
func TestAdaptiveUnitTickSimulation(t *testing.T) {
	t.Parallel()
	type regime struct {
		name        string
		totalZones  int
		latencyFn   func(tick, lotZones int) time.Duration
		wantMinLots int
		wantMaxLots int
		wantEndSize int
		wantMaxOsc  int // direction reversals after initial convergence
	}
	regimes := []regime{
		{
			name:       "fast_nvme_100k_zones",
			totalZones: 100_000,
			// 12 ms fixed fsync + 0.08 ms/zone -> stabilizes in deadband at 128 zones (~22 ms).
			latencyFn: func(_ int, n int) time.Duration {
				return 12*time.Millisecond + time.Duration(n)*80*time.Microsecond
			},
			wantMinLots: 775,
			wantMaxLots: 795,
			wantEndSize: 128,
			wantMaxOsc:  0,
		},
		{
			name:       "slow_loaded_disk",
			totalZones: 5_000,
			// 20 ms fixed + 8 ms/zone -> backs off from 16 (148 ms) to 4 (52 ms, inside [20, 80]).
			latencyFn: func(_ int, n int) time.Duration {
				return 20*time.Millisecond + time.Duration(n)*8*time.Millisecond
			},
			wantMinLots: 1240,
			wantMaxLots: 1255,
			wantEndSize: 4,
			wantMaxOsc:  0,
		},
		{
			name:       "noisy_equilibrium_zero_thrashing",
			totalZones: 20_000,
			// Around 40 ms at 16 zones with +-35% deterministic jitter ([26 ms, 54 ms] in deadband).
			latencyFn: func(tick, n int) time.Duration {
				base := float64(n) * 2.5 // ~40 ms at n=16
				jitter := 1.0 + 0.35*float64((tick%5)-2)/2.0
				return time.Duration(base*jitter) * time.Millisecond
			},
			wantMinLots: 1245,
			wantMaxLots: 1255,
			wantEndSize: 16,
			wantMaxOsc:  0,
		},
	}

	for _, tc := range regimes {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdaptiveUnit(0)
			done, lots, prevDir, reversals := 0, 0, 0, 0
			for done < tc.totalZones {
				n := min(s.Current(), tc.totalZones-done)
				prev := s.Current()
				s.Observe(n, tc.latencyFn(lots, n))
				next := s.Current()
				dir := 0
				if next > prev {
					dir = 1
				} else if next < prev {
					dir = -1
				}
				if dir != 0 {
					if prevDir != 0 && dir != prevDir {
						reversals++
					}
					prevDir = dir
				}
				done += n
				lots++
			}
			if lots < tc.wantMinLots || lots > tc.wantMaxLots {
				t.Fatalf("lots=%d want [%d, %d] (endSize=%d)", lots, tc.wantMinLots, tc.wantMaxLots, s.Current())
			}
			if s.Current() != tc.wantEndSize {
				t.Fatalf("endSize=%d want %d", s.Current(), tc.wantEndSize)
			}
			if reversals > tc.wantMaxOsc {
				t.Fatalf("direction reversals=%d want <= %d", reversals, tc.wantMaxOsc)
			}
			t.Logf("%s: zones=%d lots=%d endSize=%d reversals=%d", tc.name, tc.totalZones, lots, s.Current(), reversals)
		})
	}

	// Load shock and recovery: fast -> slow shock -> fast recovery.
	t.Run("load_shock_and_recovery", func(t *testing.T) {
		s := newAdaptiveUnit(0)
		// Phase 1: fast disk (30 ticks) -> stabilizes at 128 (~20 ms in [20, 80]).
		for range 30 {
			n := s.Current()
			s.Observe(n, 10*time.Millisecond+time.Duration(n)*80*time.Microsecond)
		}
		if got := s.Current(); got != 128 {
			t.Fatalf("phase 1 fast: size=%d want 128", got)
		}
		// Phase 2: sudden disk stall (30 ticks) -> backs off cleanly to 4 (68 ms in [20, 80]).
		for range 30 {
			n := s.Current()
			s.Observe(n, 20*time.Millisecond+time.Duration(n)*12*time.Millisecond)
		}
		if got := s.Current(); got != 4 {
			t.Fatalf("phase 2 shock: size=%d want 4", got)
		}
		// Phase 3: disk recovers (30 ticks) -> ramps back to 128 symmetrically.
		for range 30 {
			n := s.Current()
			s.Observe(n, 10*time.Millisecond+time.Duration(n)*80*time.Microsecond)
		}
		if got := s.Current(); got != 128 {
			t.Fatalf("phase 3 recovery: size=%d want 128", got)
		}
	})
}

// A lot whose cost does not depend on its size (commits and one round trip on
// a saturated receiver) is not made smaller: halving it would only multiply
// the lots. AWS 2026-11-26: lots fell to 4 zones at 267 ms each.
func TestAdaptiveUnitKeepsItsSizeWhenHalvingDoesNotPay(t *testing.T) {
	s := newAdaptiveUnit(0)
	start := s.Current()
	for i := range 20 {
		s.Observe(s.Current(), 300*time.Millisecond)
		if got := s.Current(); got < start/2 {
			t.Fatalf("observation %d: size %d under a fixed 300 ms hold, started at %d", i, got, start)
		}
	}
	if got := s.Current(); got != start {
		t.Fatalf("size %d after a fixed hold, want %d back", got, start)
	}
	// Conditions change: fast lots grow again, and a hold that follows the
	// size is halved again.
	s.Observe(s.Current(), 5*time.Millisecond)
	if got := s.Current(); got != 2*start {
		t.Fatalf("size %d after a fast lot, want %d", got, 2*start)
	}
	for range 10 {
		s.Observe(s.Current(), time.Duration(s.Current())*25*time.Millisecond)
	}
	if got := s.Current(); got != flipUnitMinZones {
		t.Fatalf("size %d under a hold of 25 ms a zone, want %d", got, flipUnitMinZones)
	}
}

// GRAIN-1: the byte budget of a grain follows the regional freeze. Only a
// grain that filled its budget can raise it.
func TestGrainBudgetFollowsTheRegionalFreeze(t *testing.T) {
	s := &Service{}
	budget := s.grainBudget()
	if got := budget.Current(); got != grainStartKiB {
		t.Fatalf("budget starts at %d KiB, want %d", got, grainStartKiB)
	}
	// One-record zones: 512 zones hold far less than the budget.
	for range 5 {
		s.observeGrain(130, 10*time.Millisecond)
	}
	if got := budget.Current(); got != grainStartKiB {
		t.Fatalf("a grain cut by the zone bound moved the budget to %d KiB", got)
	}
	// Full grains with a short freeze raise it, up to the maximum.
	for range 10 {
		s.observeGrain(budget.Current(), 20*time.Millisecond)
	}
	if got := budget.Current(); got != grainMaxKiB {
		t.Fatalf("budget %d KiB after short freezes, want %d", got, grainMaxKiB)
	}
	// A freeze that follows the size lowers it again, not below the minimum.
	for range 12 {
		s.observeGrain(budget.Current(), time.Duration(budget.Current())*time.Millisecond)
	}
	if got := budget.Current(); got != grainMinKiB {
		t.Fatalf("budget %d KiB after long freezes, want %d", got, grainMinKiB)
	}
	// A resumed transfer has no grain: it is not an observation.
	s.observeGrain(0, time.Second)
	if got := budget.Current(); got != grainMinKiB {
		t.Fatalf("budget %d KiB after a transfer without grain", got)
	}
}
