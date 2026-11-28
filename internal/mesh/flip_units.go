package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// A granular handoff copies and flips authority in small units (FLIP-UNIT):
//
//   - Units adapt from 4 to 2048 zones of one depth, neighbours in
//     collection and location order, further bounded by transferLotBytes
//     (1 MiB, or one zone up to 8 MiB) before fencing. Only its zones have no writer while it flips.
//   - Deeper zones flip first, so a parent flips after every child of the
//     operation (ZONE-2).
//   - Within a depth, already-frozen zones finish first on retry, then the
//     most written units go first (Service.Heat): the donor sheds write load
//     from the first lots on, before the rest of the region is even copied.
//
// Each zone keeps its own durable state: a handoff cut anywhere resumes from
// the zones not yet transferred, whatever units they were in.
//
// Service.FlipUnitZones and FlipUnitsInFlight override the defaults.
const (
	flipUnitZones    = 16
	flipUnitMinZones = 4
	flipUnitMaxZones = 2048
	// flipUnitHoldGoal is the target time a lot's zones stay without a
	// writer (FLIP-HOLD-1). A write to one of them waits that long, so the
	// goal is a write-latency budget, not a throughput setting: lots shrink
	// above twice the goal (80 ms) and grow below half of it (20 ms). A
	// smaller goal means more lots, more fsyncs and a longer handoff.
	flipUnitHoldGoal  = 40 * time.Millisecond
	flipUnitsInFlight = 1
	// cutoverWaitLimit bounds how long a write hitting ErrFrozen waits for an
	// active local flipUnit to finish before falling back to 503.
	cutoverWaitLimit = 500 * time.Millisecond
)

func (s *Service) beginCutover(keys []tree.ZoneKey) chan struct{} {
	done := make(chan struct{})
	s.cutoverMu.Lock()
	if s.cutovers == nil {
		s.cutovers = make(map[tree.Key]chan struct{}, len(keys))
	}
	for _, z := range keys {
		s.cutovers[z.Key()] = done
	}
	s.cutoverMu.Unlock()
	return done
}

func (s *Service) endCutover(keys []tree.ZoneKey, done chan struct{}) {
	s.cutoverMu.Lock()
	for _, z := range keys {
		k := z.Key()
		if s.cutovers[k] == done {
			delete(s.cutovers, k)
		}
	}
	s.cutoverMu.Unlock()
	close(done)
}

type regionCutover struct {
	region tree.Region
	done   chan struct{}
}

func (s *Service) beginRegionCutover(id causal.TransferID, region tree.Region) chan struct{} {
	done := make(chan struct{})
	s.cutoverMu.Lock()
	defer s.cutoverMu.Unlock()
	if s.regionCutovers == nil {
		s.regionCutovers = map[causal.TransferID]regionCutover{}
	}
	s.regionCutovers[id] = regionCutover{region: region, done: done}
	return done
}
func (s *Service) endRegionCutover(id causal.TransferID, done chan struct{}) {
	s.cutoverMu.Lock()
	if current, exists := s.regionCutovers[id]; exists && current.done == done {
		delete(s.regionCutovers, id)
	}
	s.cutoverMu.Unlock()
	close(done)
}

// Only a selected lot pauses new writes during preparation. Regional signals
// span the warm copy too, and are used solely after a durable refusal.
func (s *Service) lotCutoverSignal(z tree.ZoneKey) chan struct{} {
	s.cutoverMu.Lock()
	defer s.cutoverMu.Unlock()
	return s.cutovers[z.Key()]
}

func (s *Service) cutoverSignal(z tree.ZoneKey) chan struct{} {
	s.cutoverMu.Lock()
	done := s.cutovers[z.Key()]
	if done == nil {
		for _, scope := range s.regionCutovers {
			if scope.region.Contains(z.ID) {
				done = scope.done
				break
			}
		}
	}
	s.cutoverMu.Unlock()
	return done
}

func (s *Service) waitCutover(ctx context.Context, z tree.ZoneKey) bool {
	return awaitCutover(ctx, s.cutoverSignal(z))
}

func awaitCutover(ctx context.Context, done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	timer := time.NewTimer(cutoverWaitLimit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// adaptiveUnit sizes a unit of work (zones of a flip lot, KiB of a handoff
// grain) from how long the last units took, around a goal: the size doubles
// when a full unit took less than half the goal and halves when one took
// more than twice the goal. Between the two it stays put, so it does not
// oscillate.
//
// A unit also has a cost that does not depend on its size (commits, one
// round trip). When that cost alone is over the limit, halving gains
// nothing and only multiplies the units: a halving that shortened the unit
// by less than a quarter is undone, and the size it came from becomes the
// floor until a fast unit shows conditions changed (AWS 2026-11-26: lots of
// 4 zones at 267 ms each, three times less data moved per second).
type adaptiveUnit struct {
	mu      sync.Mutex
	size    int
	minSize int
	maxSize int
	goal    time.Duration
	fixed   bool
	floor   int           // no halving below it; 0: none
	before  time.Duration // what the unit took at twice the size, when the last step halved it
}

func newAdaptiveUnit(override int) *adaptiveUnit {
	if override > 0 {
		return &adaptiveUnit{size: override, minSize: override, maxSize: override, fixed: true}
	}
	return &adaptiveUnit{
		size:    flipUnitZones,
		minSize: flipUnitMinZones,
		maxSize: flipUnitMaxZones,
		goal:    flipUnitHoldGoal,
	}
}

func (a *adaptiveUnit) Current() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.size
}

// Observe records that a unit of n took held.
func (a *adaptiveUnit) Observe(n int, held time.Duration) {
	if a == nil || a.fixed || held <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	halved := a.before
	a.before = 0
	switch {
	case held < a.goal/2:
		a.floor = 0
		if n >= a.size {
			a.size = min(a.maxSize, a.size*2)
		}
	case held <= a.goal*2:
	case halved > 0 && held*4 > halved*3:
		a.size = min(a.maxSize, a.size*2) // the halving did not pay: undo it
		a.floor = a.size
	case a.size > max(a.minSize, a.floor):
		a.before = held
		a.size = max(a.minSize, a.size/2)
	}
}

// zoneDepth is how deep z is in its collection: 0 for the root.
func zoneDepth(z tree.ZoneKey) int {
	if z.Location == "" || z.Location == tree.BASE64.Root() {
		return 0
	}
	return len(z.Location)
}

// flipUnits orders zones bottom-up: levels deepest first, each a list of
// bounded units sorted by descending write heat (stable on location order).
func flipUnits(zones []tree.ZoneKey, heat func(tree.Key) int64, size int) [][][]tree.ZoneKey {
	if size < 1 {
		size = 1
	}
	sorted := append([]tree.ZoneKey(nil), zones...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if da, db := zoneDepth(a), zoneDepth(b); da != db {
			return da > db
		}
		return a.Compare(b) < 0
	})
	var levels [][][]tree.ZoneKey
	for start := 0; start < len(sorted); {
		depth := zoneDepth(sorted[start])
		var units [][]tree.ZoneKey
		for start < len(sorted) && zoneDepth(sorted[start]) == depth {
			end := start + 1
			for end < len(sorted) && end-start < size && zoneDepth(sorted[end]) == depth {
				end++
			}
			units = append(units, sorted[start:end])
			start = end
		}
		if heat != nil {
			warmth := make([]int64, len(units))
			order := make([]int, len(units))
			for i, unit := range units {
				order[i] = i
				for _, z := range unit {
					warmth[i] += heat(z.Key())
				}
			}
			sort.SliceStable(order, func(i, j int) bool { return warmth[order[i]] > warmth[order[j]] })
			byHeat := make([][]tree.ZoneKey, len(units))
			for i, from := range order {
				byHeat[i] = units[from]
			}
			units = byHeat
		}
		levels = append(levels, units)
	}
	return levels
}

// flipUnit copies and flips one unit in byte-bounded lots: warm copy and delta
// catch-up while writable, then freeze, last delta, seal certification, emit
// and install. Each lot's no-writer window is recorded on progress.
func (s *Service) flipUnit(ctx context.Context, transfer storage.CausalTransfer, unit []tree.ZoneKey, known *flipKnown, progress *transferProgress, sizer *adaptiveUnit) error {
	for start := 0; start < len(unit); {
		lotCtx := s.jobs.copies.traceWarmLot(ctx, known.urgent)
		began := time.Now()
		local := known.ofKeys(unit[start:])
		metrics.HandoffPhase(transfer.Cession.ID.String(), "warm-copy")
		keys, err := s.copyLot(lotCtx, transfer, unit[start:], local, !known.urgent)
		known.merge(local)
		if err != nil {
			return fmt.Errorf("warm-lot: %w", err)
		}
		if len(keys) == 0 {
			return nil
		}
		held, err := s.cutoverLot(lotCtx, transfer, keys, local, known, progress)
		if held > 0 {
			progress.held(held)
			sizer.Observe(len(keys), held)
		}
		if err != nil {
			return err
		}
		if err := s.jobs.copies.pace(lotCtx, progress.operation, time.Since(began), known.urgent); err != nil {
			return err
		}
		start += len(keys)
	}
	return nil
}

// cutoverLot freezes, stages, emits, installs, and acknowledges one lot under
// a single deferred cutover barrier so every exit path (including panics)
// releases waiting writers.
func (s *Service) cutoverLot(ctx context.Context, transfer storage.CausalTransfer, keys []tree.ZoneKey, local map[string]uint64, known *flipKnown, progress *transferProgress) (held time.Duration, err error) {
	operation := transfer.Cession.ID
	target := transfer.Cession.Target
	fence := func() ([]storage.Snapshot, []storage.ZoneSeal, []causal.ZoneTransfer, error) {
		return s.Store.FenceZones(operation, keys)
	}
	if cold, _ := ctx.Value(coldFoldContextKey{}).(bool); cold && !known.urgent {
		metrics.HandoffPhase(operation.String(), "prepare-cold-fence")
		fence, err = s.Store.PrepareColdFence(operation, keys)
		if err != nil {
			return 0, err
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
	began := time.Now()
	mark := began
	cutoverDone := s.beginCutover(keys)
	phase := "freeze"
	setPhase := func(next string) { phase = next; metrics.HandoffPhase(operation.String(), next) }
	setPhase(phase)
	var once sync.Once
	release := func() { once.Do(func() { s.endCutover(keys, cutoverDone) }) }
	defer func() {
		if err != nil || held == 0 {
			held = time.Since(began)
		}
		release() // release writers before diagnostics
		if err != nil {
			metrics.Handoffs.CutoverFailed.Observe(held)
		} else {
			metrics.Handoffs.CutoverCall.Observe(held)
		}
		if held >= time.Second || err != nil {
			slog.Warn("zone cutover delayed", "operation", operation, "target", target.Host,
				"zones", len(keys), "phase", phase, "elapsed", held, "error", err)
		}
	}()

	snaps, seals, zts, err := fence()
	fencedAt := time.Now()
	mark = progress.add("freeze", mark, 0)
	if err != nil {
		return 0, fmt.Errorf("freeze-unit: %w", err)
	}
	part := wireTransfer(transfer)
	part.Snapshots = snaps
	// The last delta (mutations after the receiver's acknowledged copy, up to
	// the fence) travels with the install: no round trip of its own.
	setPhase("mirror")
	delta, err := s.lastDelta(part, local)
	mark = progress.add("mirror", mark, 0)
	if err != nil {
		return 0, fmt.Errorf("mirror-unit: %w", err)
	}
	// One round trip, one commit at the receiver: certify against the seals
	// the fence computed and mount (FLIP-ONE-1).
	setPhase("install")
	_, err = s.retryZoneInstall(ctx, target, Request{Kind: "install-zone", Operation: operation, Owner: s.Self, Batch: delta, Seals: seals, ZoneTransfers: zts})
	if err == nil {
		for _, snap := range snaps {
			local[zoneKey(snap)] = snap.Seq
		}
		known.merge(local)
	}
	if errors.Is(err, storage.ErrSequence) || errors.Is(err, storage.ErrNotReady) {
		// The receiver's mirror does not match the fenced seal: send the
		// fenced snapshot itself, verified against the same seal there.
		_, err = s.retryZoneInstall(ctx, target, Request{Kind: "install-zone", Operation: operation, Transfer: &part, ZoneTransfers: zts})
	}
	held = time.Since(began)
	if err == nil {
		ackDelay := time.Since(fencedAt)
		metrics.Handoffs.FreezeReturnToInstallACK.Observe(ackDelay)
		// The receiver writes these zones now: waiting writers go to it
		// without waiting for this disk's ACK bookkeeping.
		release()
		if slog.Default().Enabled(ctx, slog.LevelDebug) {
			slog.DebugContext(ctx, "zone cutover", "freeze_call_us", fencedAt.Sub(began).Microseconds(),
				"fence_to_install_ack_us", ackDelay.Microseconds())
		}
		mark = progress.add("install", mark, len(keys))
		setPhase("ack")
		err = s.Store.AckZoneInstalls(operation, keys)
		if err == nil {
			s.membershipTransport().schedule(target.Host, s.membershipSend)
		}
	}
	progress.add(phase, mark, 0)
	if err != nil {
		return 0, fmt.Errorf("install-unit: %w", err)
	}
	return held, nil
}

// lastDelta reads the mutations between the receiver's acknowledged copy of
// each fenced zone and its fence. The copied snapshots are byte-bounded;
// the final mutation payload does not yet have an atomic admission budget.
func (s *Service) lastDelta(offer storage.CausalTransfer, known map[string]uint64) ([]storage.ZoneMutations, error) {
	var ranges []storage.MutationRange
	for _, snap := range offer.Snapshots {
		after, ok := known[zoneKey(snap)]
		if !ok || after > snap.Seq {
			return nil, storage.ErrSequence
		}
		if after < snap.Seq {
			ranges = append(ranges, storage.MutationRange{Zone: snap.Zone, After: after, Through: snap.Seq})
		}
	}
	if len(ranges) == 0 {
		return nil, nil
	}
	return s.Store.MutationsBatch(ranges)
}

// flipLevel flips the zones of one depth, hottest first, in one of three
// ways: FlipUnitsInFlight units at a time when it is above one; otherwise
// one lot after another, re-cut to the size the sizer learns from each
// cutover when it is adaptive (FlipUnitZones == 0), or as the units given.
func (s *Service) flipLevel(ctx context.Context, transfer storage.CausalTransfer, units [][]tree.ZoneKey, known *flipKnown, progress *transferProgress, sizer *adaptiveUnit) error {
	flight := s.FlipUnitsInFlight
	if flight < 1 {
		flight = flipUnitsInFlight
	}
	flip := func(unit []tree.ZoneKey) error {
		return s.flipUnit(ctx, transfer, unit, known, progress, sizer)
	}
	switch {
	case flight > 1:
		return flipInFlight(ctx, units, flight, flip)
	case sizer != nil && !sizer.fixed:
		return flipResized(ctx, slices.Concat(units...), sizer, flip)
	}
	for _, unit := range units {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := flip(unit); err != nil {
			return err
		}
	}
	return nil
}

// flipResized flips zones in order, each lot of the size the sizer gives
// after the previous one.
func flipResized(ctx context.Context, zones []tree.ZoneKey, sizer *adaptiveUnit, flip func([]tree.ZoneKey) error) error {
	for len(zones) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(sizer.Current(), len(zones))
		if err := flip(zones[:n]); err != nil {
			return err
		}
		zones = zones[n:]
	}
	return nil
}

// flipInFlight flips units, flight of them at a time, and starts no new one
// after the first failure. It returns that failure, else the context's.
func flipInFlight(ctx context.Context, units [][]tree.ZoneKey, flight int, flip func([]tree.ZoneKey) error) error {
	var (
		wait   sync.WaitGroup
		mu     sync.Mutex
		failed error
	)
	failure := func() error {
		mu.Lock()
		defer mu.Unlock()
		return failed
	}
	slots := make(chan struct{}, flight)
	for _, unit := range units {
		slots <- struct{}{}
		if failure() != nil || ctx.Err() != nil {
			<-slots
			break
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer func() { <-slots }()
			err := flip(unit)
			if err == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if failed == nil {
				failed = err
			}
		}()
	}
	wait.Wait()
	if failed != nil {
		return failed
	}
	return ctx.Err()
}

// flipKnown shares the receiver's progress per zone between the units of a
// level: each unit works on the entries of its own zones.
type flipKnown struct {
	urgent bool // immutable during one plan pass; unfinished fences have priority
	mu     sync.Mutex
	known  map[string]uint64
}

func (k *flipKnown) ofKeys(keys []tree.ZoneKey) map[string]uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	local := make(map[string]uint64, len(keys))
	for _, z := range keys {
		key := zoneKeyOf(z)
		if after, ok := k.known[key]; ok {
			local[key] = after
		}
	}
	return local
}

func (k *flipKnown) merge(local map[string]uint64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for zone, after := range local {
		k.known[zone] = after
	}
}
