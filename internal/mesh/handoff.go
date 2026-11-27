package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func handoffKey(r Request) string {
	if r.Operation.Valid() {
		return r.Operation.String()
	}
	return fmt.Sprintf("proposal:%x", causal.Hash(struct {
		Input  causal.Digest
		Target causal.Endpoint
		Region tree.Region
	}{r.Input, r.Target, r.Region}))
}

// A gate protects one installed input, not membership or unrelated rights.
func (s *Service) handoff(ctx context.Context, r Request) error {
	var lane *os.File
	originalKey := handoffKey(r)
	state, err := s.Store.CausalState()
	if err != nil {
		return err
	}
	if r.Operation.Valid() {
		tr, found := state.Transfers[r.Operation]
		if !found {
			return storage.ErrNotReady
		}
		if tr.Cession.Source.Owner != s.Self.Owner {
			return storage.ErrNotOwner
		}
		r.Input = tr.Cession.Input.ID // never gate an operation by a caller's stale input
	} else {
		if r.Target.VNode == s.Self.VNode && r.Target.Owner != s.Self.Owner && (state.Moving == nil || *state.Moving != r.Target) {
			return storage.ErrNotReady
		}
		m, e := state.MembershipReplica()
		if e != nil {
			return e
		}
		for _, member := range m.Export().Members {
			if member.VNode.Name == r.Target.VNode && member.Retired {
				return causal.ErrConflict
			}
		}
		// Resume a real pinned predecessor before choosing any replacement. This
		// also recovers an abandoned job without a background maintenance worker.
		for id, tr := range state.Transfers {
			c := tr.Cession
			if c.Source.Owner != s.Self.Owner || c.Input.ID != r.Input || tr.Phase == causal.TransferInstalled || tr.Phase == causal.TransferCancelled {
				continue
			}
			if err = s.handoff(ctx, Request{Kind: "handoff", Operation: id, Input: c.Input.ID, Target: c.Target, Region: c.Region}); err != nil {
				return err
			}
			return storage.ErrConflict
		}
		if _, exists := state.Permissions.Find(r.Input); !exists {
			return storage.ErrConflict
		}
		if r.Subdivide {
			lane, err = s.jobs.copies.reserveFile(ctx, "", copySlotWait, true)
			if err != nil {
				return err
			}
			defer releaseCopyFile(lane)
			budget := s.grainBudget()
			var held int64
			r.Region, held, err = s.Store.HandoffGrainBytes(ctx, r.Region, handoffGrainZones, int64(budget.Current())<<10)
			if err != nil {
				return err
			}
			r.grainKiB = int(held >> 10)
			offer, e := s.Store.BeginCausalPiece(ctx, r.Input, r.Target, r.Region)
			if e != nil {
				return e
			}
			r.Operation = offer.Cession.ID
			r.Input, r.Region = offer.Cession.Input.ID, offer.Cession.Region
			s.jobs.bindRight(originalKey, r.Input)
		}
	}
	// The actual installed piece is the only gate. Its local complements were
	// already released by BeginCausalPiece, before admission, bytes or any fence.
	release, err := s.handoffRights.acquire(ctx, r.Input)
	if err != nil {
		return err
	}
	defer release()
	return s.handoffLocked(ctx, r, lane)
}

// handoffLocked runs one transfer from wherever its durable phase left it:
//
//	transferOffer     find the transfer or begin it, zones discovered
//	(cancelled)       finish the cancellation on both sides
//	(installed)       collect what it left behind
//	(issued)          finishIssued: install at the target, complete
//	copySlot          a slot of the host's copy budget, held to the end
//	admitTransfer     ticket bound at the target, receipt confirmed
//	fenceRegion       zones moved lot by lot until the region freezes
//	consolidateRegion seal, stage, emit, install, complete
//
// Every phase is durable before the next begins, so a crash resumes here.
func (s *Service) handoffLocked(ctx context.Context, r Request, lane *os.File) (err error) {
	state, err := s.Store.CausalState()
	if err != nil {
		return err
	}
	if !r.Operation.Valid() {
		// A crashed predecessor can still pin this input. Finish its exact durable
		// decision before replanning; never choose another successor for a fence.
		if id, pending, found := s.pendingFrom(state, r.Input); found {
			c := pending.Cession
			if c.Target != r.Target || c.Region != r.Region {
				if e := s.handoffLocked(ctx, Request{Kind: "handoff", Operation: id, Input: c.Input.ID, Target: c.Target, Region: c.Region}, nil); e != nil {
					return e
				}
				return storage.ErrConflict
			}
			r.Operation = id
		}
	}
	// A full host defers proposals while their rights are still unpinned.
	if !r.Operation.Valid() {
		lane, err = s.jobs.copies.reserveFile(ctx, "", copySlotWait, true)
		if err != nil {
			return err
		}
		defer releaseCopyFile(lane)
	}
	offer, err := s.transferOffer(ctx, state, r)
	if err != nil {
		return err
	}
	c := offer.Cession
	id := c.ID
	metrics.HandoffQueued(id.String(), c.Target.Host)
	defer func() { err = s.endHandoff(ctx, id, c, err) }()

	switch {
	case offer.Phase == causal.TransferCancelled:
		return s.finishCancellation(ctx, c)
	case offer.Phase == causal.TransferInstalled:
		metrics.HandoffPhase(id.String(), "collect")
		return s.Store.CollectTransferred(id)
	}
	if lane != nil {
		// Name the already-held lane before the first preload to a local receiver.
		if _, err = lane.WriteAt([]byte(id.String()), 0); err != nil {
			return err
		}
	} else if offer.Phase < causal.TransferIssued {
		// One slot of the host for the whole grain, taken before admission
		// and before any fence (HOST-COPY-1).
		release, e := s.copySlot(ctx, offer)
		if e != nil {
			return e
		}
		defer release()
	}
	metrics.HandoffStarted(id.String(), c.Target.Host)
	scopeDone := s.beginRegionCutover(id, c.Region)
	defer s.endRegionCutover(id, scopeDone)
	if offer.Phase >= causal.TransferIssued {
		return s.finishIssued(ctx, id, c)
	}
	offer, err = s.admitTransfer(ctx, offer)
	c = offer.Cession // with its ticket, also when the admission then failed
	if err != nil {
		return err
	}
	known := s.jobs.progress(id)
	offer, frozen, err := s.fenceRegion(ctx, offer, known)
	if err != nil {
		return err
	}
	if c, err = s.consolidateRegion(ctx, offer, c); err != nil {
		return err
	}
	s.observeGrain(r.grainKiB, time.Since(frozen))
	return nil
}

// A handoff grain is the sub-region one transfer delegates: an address
// prefix holding at most handoffGrainZones zones and a byte budget (GRAIN-1).
// The budget follows the regional freeze that ends each transfer, the time
// from the region's freeze to its installed right, during which a write to
// an address of the grain that has not moved yet waits. It starts at
// grainStartKiB, doubles while a full grain freezes for less than half
// grainFreezeGoal and halves beyond twice that goal.
//
// The zone bound does not adapt. Measured on 2026-11-26 with one-record
// zones: under 512 zones the control commits multiply (join 93 s at 512,
// 214 s at 128), over 1 024 the tail of write latency degrades (p99 180 ms
// at 512, 334 ms at 4 096).
const (
	grainStartKiB   = 1 << 10 // 1 MiB
	grainMinKiB     = 1 << 8  // 256 KiB
	grainMaxKiB     = 1 << 14 // 16 MiB
	grainFreezeGoal = 100 * time.Millisecond
)

func (s *Service) grainBudget() *adaptiveUnit {
	s.grainOnce.Do(func() {
		s.grain = &adaptiveUnit{size: grainStartKiB, minSize: grainMinKiB, maxSize: grainMaxKiB, goal: grainFreezeGoal}
	})
	return s.grain
}

// observeGrain feeds the budget with a finished grain. A grain that filled
// less than half its budget was cut by the zone bound or by the region
// itself: it says nothing about a larger budget, so it cannot raise it.
func (s *Service) observeGrain(heldKiB int, frozen time.Duration) {
	if heldKiB <= 0 {
		return // a resumed transfer, or one that was not cut into grains
	}
	budget := s.grainBudget()
	if n := budget.Current(); heldKiB*2 >= n {
		heldKiB = n
	}
	budget.Observe(heldKiB, frozen)
}

// pendingFrom is the unfinished transfer this disk already began from input.
func (s *Service) pendingFrom(state storage.CausalState, input causal.Digest) (causal.TransferID, storage.CausalTransfer, bool) {
	for id, pending := range state.Transfers {
		finished := pending.Phase == causal.TransferInstalled || pending.Phase == causal.TransferCancelled
		if pending.Cession.Source.Owner == s.Self.Owner && pending.Cession.Input.ID == input && !finished {
			return id, pending, true
		}
	}
	return causal.TransferID{}, storage.CausalTransfer{}, false
}

// transferOffer is the transfer the request names, or a new one, with its
// zone list complete unless it is already sealed.
func (s *Service) transferOffer(ctx context.Context, state storage.CausalState, r Request) (offer storage.CausalTransfer, err error) {
	if !r.Operation.Valid() {
		if offer, err = s.Store.BeginCausal(ctx, r.Input, r.Target, r.Region); err != nil {
			return offer, err
		}
	} else {
		var ok bool
		if offer, ok = state.Transfers[r.Operation]; !ok {
			return offer, storage.ErrNotReady
		}
		if offer.Cession.Source.Owner != s.Self.Owner {
			return offer, storage.ErrNotOwner
		}
	}
	if offer.Discovered || offer.Phase >= causal.TransferSealed {
		return offer, nil
	}
	c := offer.Cession
	return s.Store.BeginCausal(ctx, c.Input.ID, c.Target, c.Region)
}

// endHandoff closes one run of a handoff. A receiver that closed or has no
// room aborts the transfer, which then reports a conflict; the job is
// forgotten once the transfer is gone or installed.
func (s *Service) endHandoff(ctx context.Context, id causal.TransferID, c causal.Cession, err error) error {
	if errors.Is(err, storage.ErrReceiverClosed) || errors.Is(err, storage.ErrDataCapacity) {
		if s.Store.AbortCausal(id) == nil && s.finishCancellation(ctx, c) == nil {
			err = storage.ErrConflict
		}
	}
	done := false
	if latest, e := s.Store.CausalState(); e == nil {
		tr, exists := latest.Transfers[id]
		done = !exists || tr.Phase == causal.TransferInstalled
	}
	if err != nil {
		slog.Warn("handoff interrupted", "operation", id, "target", c.Target.Host, "err", err)
	}
	metrics.HandoffEnded(id.String(), done)
	if done {
		s.jobs.forget(id)
	}
	return err
}

// finishIssued ends a transfer whose cession is issued: the target installs
// it unless it already did, then the source completes.
func (s *Service) finishIssued(ctx context.Context, id causal.TransferID, c causal.Cession) error {
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return err
	}
	if !routing.ReceiverDrained(c.Target) {
		metrics.HandoffPhase(id.String(), "install-region")
		if _, err = s.endpointCall(ctx, c.Target, Request{Kind: "install", Cession: &c}); err != nil {
			return err
		}
	}
	metrics.HandoffPhase(id.String(), "complete-region")
	if err = s.Store.CompleteCausal(id); err == nil {
		metrics.Handoffs.Given.Add(1)
	}
	return err
}

// admitTransfer obtains the target's ticket and its durable receipt. Even an
// empty address range gets this receipt before any fence: the header-only
// first preload confirms the ticket the receiver reserved.
func (s *Service) admitTransfer(ctx context.Context, offer storage.CausalTransfer) (storage.CausalTransfer, error) {
	c := offer.Cession
	id := c.ID
	if c.Ticket == 0 {
		response, err := s.endpointCall(ctx, c.Target, Request{Kind: "admit", Cession: &c})
		if err != nil {
			return offer, err
		}
		if response.Cession == nil || response.Cession.Binding() != c.Binding() {
			return offer, causal.ErrConflict
		}
		c = *response.Cession
		offer.Cession = c // the caller reports and cancels with the ticket
		if err = s.Store.BindCausal(id, c.Ticket); err != nil {
			return offer, err
		}
	}
	if offer.Phase >= causal.TransferReady {
		return offer, nil
	}
	part := wireTransfer(offer)
	if _, err := s.endpointCall(ctx, c.Target, Request{Kind: "preload", Transfer: &part}); err != nil {
		if errors.Is(err, storage.ErrNotReady) {
			return offer, fmt.Errorf("%w: tentative admission no longer open", storage.ErrReceiverClosed)
		}
		return offer, err
	}
	return offer, s.Store.ConfirmCausal(id)
}

// maxWarmCloseDrains bounds the passes a region is drained before its freeze
// is forced.
const maxWarmCloseDrains = 6

// fenceRegion moves the region's zones, pass after pass, until the region
// can be frozen: no zone is left to move. The freeze is forced after
// maxWarmCloseDrains passes, or when new zones keep arriving as fast as they
// leave. The zones that arrived before the freeze are moved last.
func (s *Service) fenceRegion(ctx context.Context, offer storage.CausalTransfer, known map[string]uint64) (_ storage.CausalTransfer, frozen time.Time, _ error) {
	id := offer.Cession.ID
	var err error
	prevMoved := -1
	for drain := 0; ; drain++ {
		if err = s.resumeZoneInstalls(ctx, offer); err != nil {
			return offer, frozen, err
		}
		moved, e := s.transferZones(ctx, id, known)
		if e != nil {
			return offer, frozen, fmt.Errorf("transfer-zones: %w", e)
		}
		if drain >= maxWarmCloseDrains || drain >= 2 && moved > flipUnitZones && moved >= prevMoved*3/4 {
			offer, err = s.Store.ForceFreezeRegion(id)
			break
		}
		prevMoved = moved
		metrics.HandoffPhase(id.String(), "fence-region")
		offer, err = s.Store.FreezeRegion(id)
		if !errors.Is(err, storage.ErrNotReady) || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return offer, frozen, fmt.Errorf("freeze-region: %w", err)
	}
	frozen = time.Now()
	if _, err = s.transferZones(ctx, id, known); err != nil {
		return offer, frozen, fmt.Errorf("transfer-stragglers: %w", err)
	}
	return offer, frozen, nil
}

// consolidateRegion turns the frozen region, whose every zone is installed
// and acknowledged, into one native right of the target. It carries only
// the receipt header, never a second copy of the data. It returns the
// cession as last known, for the handoff's closing report.
func (s *Service) consolidateRegion(ctx context.Context, offer storage.CausalTransfer, c causal.Cession) (causal.Cession, error) {
	id := c.ID
	var err error
	metrics.HandoffPhase(id.String(), "seal-region")
	if offer, err = s.Store.SealCausalRegion(id); err != nil {
		return c, fmt.Errorf("seal-region: %w", err)
	}
	metrics.HandoffPhase(id.String(), "stage-region")
	if _, err = s.endpointCall(ctx, c.Target, Request{Kind: "stage", Transfer: &offer}); err != nil {
		return c, fmt.Errorf("stage: %w", err)
	}
	metrics.HandoffPhase(id.String(), "emit-region")
	if c, err = s.Store.EmitCausal(id); err != nil {
		return c, fmt.Errorf("emit: %w", err)
	}
	metrics.HandoffPhase(id.String(), "install-region")
	if _, err = s.endpointCall(ctx, c.Target, Request{Kind: "install", Cession: &c}); err != nil {
		return c, fmt.Errorf("install: %w", err)
	}
	metrics.HandoffPhase(id.String(), "complete-region")
	if err = s.Store.CompleteCausal(id); err != nil {
		return c, err
	}
	metrics.Handoffs.Given.Add(1)
	s.membershipTransport().schedule(c.Target.Host, s.membershipSend)
	return c, nil
}

func (s *Service) hasNewPendingZone(id causal.TransferID, planned map[tree.Key]struct{}) (bool, error) {
	zones, err := s.Store.TransferZones(id)
	if err != nil {
		return false, err
	}
	for _, st := range zones {
		if !st.Transferred {
			if _, ok := planned[st.Zone.Key()]; !ok {
				return true, nil
			}
		}
	}
	return false, nil
}

// transferZones moves the zones of a transfer that are not moved yet, level
// by level, and returns how many it moved. Before the region is sealed it
// makes two passes (the zones that exist, then those born meanwhile) and
// leaves the rest to the freeze; after it, it drains every straggler.
func (s *Service) transferZones(ctx context.Context, operation causal.TransferID, known map[string]uint64) (moved int, err error) {
	progress := newTransferProgress(operation.String())
	defer func() {
		moved = progress.zones
		progress.log(err)
	}()
	shared := &flipKnown{known: known}
	sizer := newAdaptiveUnit(s.FlipUnitZones)
	size := s.FlipUnitZones
	if size < 1 {
		size = flipUnitZones
	}
	rebuilds := 0
	for pass := 0; ; {
		transfer, plan, err := s.pendingZones(operation, progress)
		if err != nil || len(plan.pending) == 0 {
			return 0, err
		}
		open := transfer.Phase < causal.TransferSealed
		shared.urgent = !open || len(plan.frozen) > 0
		for _, units := range flipUnits(plan.pending, plan.heat(s.Heat), size) {
			if err = s.flipLevel(ctx, transfer, units, shared, progress, sizer); err != nil {
				break
			}
		}
		// A child born during a parent's copy joins the MigrationSet in its
		// write transaction. Rebuild the plan only if a genuinely new zone
		// appeared, without advancing pass or spinning on other errors.
		if err != nil && open && rebuilds < maxChildRebuilds && ctx.Err() == nil && errors.Is(err, storage.ErrNotReady) {
			if added, checkErr := s.hasNewPendingZone(operation, plan.planned); checkErr == nil && added {
				rebuilds++
				continue
			}
		}
		if err != nil {
			return 0, err
		}
		if pass++; open && pass >= 2 {
			return 0, nil
		}
	}
}

// zonePlan is the zones of a transfer still to move, as one pass sees them.
type zonePlan struct {
	pending []tree.ZoneKey
	planned map[tree.Key]struct{}
	frozen  map[tree.Key]bool
}

// pendingZones reads the transfer and the zones it has not moved yet.
func (s *Service) pendingZones(operation causal.TransferID, progress *transferProgress) (storage.CausalTransfer, zonePlan, error) {
	mark := time.Now()
	state, err := s.Store.CausalState()
	progress.add("state", mark, 0)
	if err != nil {
		return storage.CausalTransfer{}, zonePlan{}, err
	}
	transfer, ok := state.Transfers[operation]
	if !ok {
		return transfer, zonePlan{}, storage.ErrNotReady
	}
	zones, err := s.Store.TransferZones(operation)
	if err != nil {
		return transfer, zonePlan{}, err
	}
	plan := zonePlan{planned: make(map[tree.Key]struct{}, len(zones)), frozen: map[tree.Key]bool{}}
	for _, st := range zones {
		if st.Transferred {
			continue
		}
		plan.pending = append(plan.pending, st.Zone)
		plan.planned[st.Zone.Key()] = struct{}{}
		if st.Frozen {
			plan.frozen[st.Zone.Key()] = true
		}
	}
	return transfer, plan, nil
}

// heat orders the plan's zones: a zone already frozen has no writer, so it
// goes before any other; the rest keep the order base gives them.
func (p zonePlan) heat(base func(tree.Key) int64) func(tree.Key) int64 {
	if len(p.frozen) == 0 {
		return base
	}
	return func(k tree.Key) int64 {
		v := int64(0)
		if base != nil {
			v = base(k)
		}
		if p.frozen[k] {
			return 1<<48 + v
		}
		return v
	}
}
