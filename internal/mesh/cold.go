package mesh

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/storage"
)

const (
	KindColdUsage   = "cold-usage"
	KindColdPin     = "cold-pin"
	KindColdCheck   = "cold-check"
	KindColdRelease = "cold-release"
	KindColdWitness = "cold-witness"
)

type ColdProposal struct {
	Operation           string
	Source              causal.Endpoint
	Bytes               int64
	MaxCombinedFraction float64
	HeatBudget          uint64
	// Items the source owns, and the most the survivor may own after the
	// fold (ColdConfig.MaxItems); zero leaves items unchecked.
	Items, MaxItems int64 `json:",omitzero"`
}

// ColdConfig bounds one pair fold (COLD-2). No ADD, global oracle or host
// drain is involved. Byte admission remains the destination's transactional
// hard gate.
//
// Defaults: the survivor stays under 45% of its capacity after the fold (a
// split fires near 90%), and up to 1024 writes are tolerated on either side
// before a new piece starts.
type ColdConfig struct {
	// Automatic asks the local machine to admit a new background fold.
	// Existing drains resume unconditionally. This flag never goes on the wire.
	Automatic           bool
	MaxCombinedFraction float64
	HeatBudget          uint64
	// MaxItems, when positive, admits a fold only if the survivor's items
	// plus the source's stay under it. With item-driven scaling it is half
	// the split threshold: a fold over the threshold is split again at once
	// (REM-124).
	MaxItems int64
}

const (
	DefaultColdCombinedFraction = 0.45
	DefaultColdHeatBudget       = 1024
)

func (c ColdConfig) defaults() ColdConfig {
	if c.MaxCombinedFraction <= 0 {
		c.MaxCombinedFraction = DefaultColdCombinedFraction
	}
	if c.HeatBudget == 0 {
		c.HeatBudget = DefaultColdHeatBudget
	}
	return c
}

// hot reports activity beyond a fold's budget.
func hot(activity, since, budget uint64) bool { return activity-since > budget }

// Compact folds this vnode into a pinned survivor whose post-remove cell covers
// it completely. This is a conservative retire-only balloon primitive. Every
// native token, including empty keyspace, is transferred using the normal
// durable handoff. Incoming pins/drains cannot cycle: a disk cannot hold both.
//
// The steps, each durable before the next:
//
//	beginColdDrain  choose the survivor and record the drain
//	pinSurvivor     the survivor accepts and pins the fold
//	drainPieces     every native right moves by a normal handoff
//	retire          the survivor still holds the pin; publish the retirement
//	witness         the survivor witnesses it; the drain is complete
func (s *Service) Compact(ctx context.Context, config ColdConfig) error {
	return s.compact(ctx, config, false)
}

func (s *Service) compact(ctx context.Context, config ColdConfig, resumeOnly bool) error {
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	config = config.defaults()
	status, err := s.Store.ColdStatus()
	if err != nil {
		return err
	}
	work := status.Drain
	if work == nil {
		// A previous attempt may have aborted while recovery waited for
		// coldMu. Recovery must never start a fresh drain in its place.
		if resumeOnly {
			return nil
		}
		// The survivor already promised this disk to another donor. A
		// maintenance tick cannot retire it until that pin is released.
		// Avoid probing a peer and reconciling summaries for a known no-op.
		if config.Automatic && status.Pin != nil {
			return ErrColdDeferred
		}
		if config.Automatic && s.jobs.copies != nil {
			release, err := s.jobs.copies.admitColdStart()
			if err != nil {
				return err
			}
			defer release()
		}
		if work, err = s.beginColdDrain(ctx, config); err != nil {
			return err
		}
	}
	if work.Witnessed {
		return nil
	}
	proposal := &ColdProposal{Operation: work.Operation, Source: work.Source, Bytes: work.Bytes, MaxCombinedFraction: work.MaxCombinedFraction, HeatBudget: work.HeatBudget}
	if work.MaxItems > 0 {
		totals, err := s.Store.PlanningTotals()
		if err != nil {
			return err
		}
		proposal.Items, proposal.MaxItems = totals.Items, work.MaxItems
	}
	if work.AbortRequested {
		return s.abortCold(ctx, *work, proposal)
	}
	if work.Retirement == nil {
		if retired, err := s.retireCold(ctx, work, proposal); err != nil || !retired {
			return err
		}
	}
	if _, err := s.call(ctx, work.Target.Host, Request{Kind: KindColdWitness, Cold: proposal, Retirement: work.Retirement}); err != nil {
		return err
	}
	if err := s.Store.CompleteColdRetirement(); err != nil {
		return err
	}
	metrics.Handoffs.Folds.Add(1)
	return nil
}

// beginColdDrain chooses the survivor and records the drain towards it.
// Geometry stays deterministic; a fresh persisted operation identity prevents
// a delayed release from an aborted trial from unpinning its successor.
func (s *Service) beginColdDrain(ctx context.Context, config ColdConfig) (*storage.ColdDrain, error) {
	snapshot, err := s.Store.CausalRouting()
	if err != nil {
		return nil, err
	}
	target, peerBytes, err := s.foldTarget(ctx, snapshot, config)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	operation := fmt.Sprintf("cold:%x", nonce)
	work, err := s.Store.BeginColdDrain(operation, target, storage.ColdLimits{MaxCombinedFraction: config.MaxCombinedFraction, HeatBudget: config.HeatBudget, MaxItems: config.MaxItems})
	if err != nil {
		return nil, err
	}
	root, _ := snapshot.Identity()
	slog.InfoContext(ctx, "cold fold started", "op", operation, "source", work.Source.VNode, "target", target.VNode, "source_owned_bytes", work.Bytes, "target_observed_bytes", peerBytes, "target_is_root", target.VNode == root.Name, "automatic", config.Automatic)
	return &work, nil
}

// foldTarget is the vnode whose cell, once this vnode is removed from the
// view, covers this vnode's cell completely. The root never folds.
func (s *Service) foldTarget(ctx context.Context, snapshot storage.RoutingSnapshot, config ColdConfig) (causal.Endpoint, int64, error) {
	var peerBytes int64 = -1 // Root and explicit folds do not probe the survivor.
	root, _ := snapshot.Identity()
	if root.Name == s.VNode.Name {
		return causal.Endpoint{}, peerBytes, protocol.ErrNoBetterConfiguration
	}
	source, peer, err := snapshot.FoldSibling(s.VNode.Name)
	if err != nil {
		return causal.Endpoint{}, peerBytes, err
	}
	if peer.Name == "" {
		return causal.Endpoint{}, peerBytes, protocol.ErrNoBetterConfiguration
	}
	target, err := snapshot.Placement(peer.Name)
	if err != nil {
		return causal.Endpoint{}, peerBytes, err
	}
	if !target.Valid() {
		return causal.Endpoint{}, peerBytes, protocol.ErrNoBetterConfiguration
	}
	if peer.Name == root.Name {
		return target, peerBytes, nil
	}
	donate := bytes.Compare(source[:], peer.ID[:]) > 0
	if config.Automatic {
		// Placement hint only. A changed pair or competing drains are still
		// checked at BeginColdDrain and PinColdSurvivor; no pin exists yet.
		local, err := s.coldUsage()
		if err != nil {
			return causal.Endpoint{}, peerBytes, err
		}
		remote, err := s.call(ctx, target.Host, Request{Kind: KindColdUsage})
		if err != nil {
			return causal.Endpoint{}, peerBytes, err
		}
		if remote.ColdUsage == nil {
			return causal.Endpoint{}, peerBytes, storage.ErrNotReady
		}
		peerBytes = remote.ColdUsage.Owned
		donate = local.shouldDonate(*remote.ColdUsage, config.MaxCombinedFraction, donate)
	}
	if !donate {
		return causal.Endpoint{}, peerBytes, protocol.ErrNoBetterConfiguration
	}
	return target, peerBytes, nil
}

// ColdUsage is the logical owned data at the disk and its capacity. Residual
// or staged copies do not move with its rights.
type ColdUsage struct{ Owned, Capacity int64 }

func (s *Service) coldUsage() (ColdUsage, error) {
	capacity, used, err := s.Store.CapacityUsage()
	return ColdUsage{Owned: used.Owned, Capacity: capacity}, err
}

func (local ColdUsage) shouldDonate(peer ColdUsage, fraction float64, tie bool) bool {
	combined := float64(local.Owned) + float64(peer.Owned)
	if combined > float64(peer.Capacity)*fraction {
		return false
	}
	if combined > float64(local.Capacity)*fraction {
		return true
	}
	if local.Owned != peer.Owned {
		return local.Owned < peer.Owned
	}
	return tie
}

// retireCold moves everything to the pinned survivor and publishes the
// retirement in work. It reports false, with the abort's outcome, when the
// fold was aborted instead.
func (s *Service) retireCold(ctx context.Context, work *storage.ColdDrain, proposal *ColdProposal) (retired bool, err error) {
	dirty, err := s.pinSurvivor(ctx, proposal, work.Target)
	if err != nil {
		return false, err
	}
	drained, err := s.drainPieces(ctx, work, proposal, dirty)
	if err != nil {
		return false, err
	}
	if !drained {
		return false, s.abortCold(ctx, *work, proposal)
	}
	// Every piece is installed: aborting now would only send the data
	// back, so heat is ignored here (COLD-2). The check still proves the
	// survivor holds our pin before the retirement is published.
	if _, err = s.call(ctx, work.Target.Host, Request{Kind: KindColdCheck, Cold: proposal}); err != nil {
		return false, err
	}
	// Exceptions include empty delegated address space. Deliver that scoped
	// directory before the retiring endpoint may disappear.
	directory, err := s.Store.ReadDirectory()
	if err != nil {
		return false, err
	}
	if _, err = s.endpointCall(ctx, work.Target, Request{Kind: "directory", Directory: directory}); err != nil {
		return false, err
	}
	record, err := s.Store.PublishColdRetirement()
	if errors.Is(err, protocol.ErrColdPlanPreempted) {
		return false, s.abortCold(ctx, *work, proposal)
	}
	if err != nil {
		return false, err
	}
	work.Retirement = &record
	if s.Activated != nil {
		s.Activated()
	}
	return true, nil
}

// pinSurvivor asks the survivor to pin the fold and records its pin. It
// returns whether the survivor is already too hot for it. Only an explicit
// remote rejection proves no pin was accepted: a lost response keeps the
// work identity durable for an idempotent retry.
func (s *Service) pinSurvivor(ctx context.Context, proposal *ColdProposal, target causal.Endpoint) (dirty bool, err error) {
	pin, err := s.call(ctx, target.Host, Request{Kind: KindColdPin, Cold: proposal})
	if err != nil {
		if errors.Is(err, storage.ErrNotReady) || errors.Is(err, storage.ErrDataCapacity) || errors.Is(err, storage.ErrConflict) {
			_ = s.Store.AbortColdDrain()
		}
		return false, err
	}
	if pin.ColdPin == nil {
		return false, storage.ErrNotReady
	}
	return pin.ColdDirty, s.Store.ConfirmColdPin(*pin.ColdPin)
}

type coldFoldContextKey struct{}

// drainPieces hands every piece of the drain to the survivor. It reports
// false when heat or a change to the pair calls the fold off before a piece
// started. A started piece is completed even then: rolling back its point
// flips or its emitted certificate would revoke acknowledged writes.
func (s *Service) drainPieces(ctx context.Context, work *storage.ColdDrain, proposal *ColdProposal, dirty bool) (drained bool, err error) {
	ctx = context.WithValue(ctx, coldFoldContextKey{}, true)
	for _, piece := range work.Pieces {
		snapshot, err := s.Store.CausalRouting()
		if err != nil {
			return false, err
		}
		phase, started := snapshot.TransferPhase(piece.Operation)
		if started && phase == causal.TransferInstalled {
			continue
		}
		if dirty, err = s.coldDirty(ctx, snapshot, work, proposal, dirty, started); err != nil {
			return false, err
		}
		if dirty && !started {
			return false, nil
		}
		if err := s.handoff(ctx, Request{Kind: "handoff", Operation: piece.Operation, Input: piece.Input, Region: piece.Region, Target: work.Target}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// coldDirty says whether the fold should be called off: it already was, or
// this disk took writes beyond its heat budget, or the pair changed, or,
// before a piece starts, the survivor reports heat (COLD-2 skips the final
// heat check, so the survivor is asked before each piece instead).
func (s *Service) coldDirty(ctx context.Context, snapshot storage.RoutingSnapshot, work *storage.ColdDrain, proposal *ColdProposal, dirty, started bool) (bool, error) {
	activity, err := s.Store.ColdActivity()
	if err != nil {
		return dirty, err
	}
	dirty = dirty || hot(activity, work.Activity, work.HeatBudget) || !snapshot.ColdPairCurrent(work)
	if dirty || started {
		return dirty, nil
	}
	checked, err := s.call(ctx, work.Target.Host, Request{Kind: KindColdCheck, Cold: proposal})
	if err != nil {
		return dirty, err
	}
	return checked.ColdDirty, nil
}

func (s *Service) abortCold(ctx context.Context, work storage.ColdDrain, proposal *ColdProposal) error {
	metrics.Handoffs.FoldsAborted.Add(1)
	// Keep the work identity until release is acknowledged; no orphan pin is
	// forgotten after a lost release ACK. Release itself is idempotent.
	if err := s.Store.RequestColdAbort(); err != nil {
		return err
	}
	if _, err := s.call(ctx, work.Target.Host, Request{Kind: KindColdRelease, Cold: proposal}); err != nil {
		return err
	}
	if err := s.Store.AbortColdDrain(); err != nil {
		return err
	}
	if s.Activated != nil {
		s.Activated()
	}
	return protocol.ErrColdPlanPreempted
}

// ResumeCold is safe after process restart; configuration and piece identities
// are durable. A receiver's pin is released only by its source's final step.
func (s *Service) ResumeCold(ctx context.Context) error {
	return s.compact(ctx, ColdConfig{}, true)
}

func (s *Service) handleCold(r Request) (out Response, err error) {
	switch r.Kind {
	case KindColdUsage:
		usage, err := s.coldUsage()
		out.ColdUsage = &usage
		return out, err
	case KindColdPin:
		if r.Cold == nil {
			return out, causal.ErrConflict
		}
		c := r.Cold
		pin, e := s.Store.PinColdSurvivor(c.Operation, c.Source, storage.ColdOffer{Bytes: c.Bytes, MaxCombinedFraction: c.MaxCombinedFraction, Items: c.Items, MaxItems: c.MaxItems})
		if e != nil {
			return out, e
		}
		out.ColdPin = &pin
		status, e := s.Store.ColdStatus()
		out.ColdDirty = hot(status.Activity, pin.Activity, c.HeatBudget)
		return out, e
	case KindColdCheck:
		if r.Cold == nil {
			return out, causal.ErrConflict
		}
		status, e := s.Store.ColdStatus()
		if e != nil {
			return out, e
		}
		if status.Pin == nil || status.Pin.Operation != r.Cold.Operation || status.Pin.Source != r.Cold.Source {
			return out, storage.ErrNotReady
		}
		out.ColdPin = status.Pin
		out.ColdDirty = hot(status.Activity, status.Pin.Activity, r.Cold.HeatBudget)
		return out, nil
	case KindColdRelease:
		if r.Cold == nil {
			return out, causal.ErrConflict
		}
		return out, s.Store.ReleaseColdPin(r.Cold.Operation, r.Cold.Source)
	case KindColdWitness:
		if r.Cold == nil || r.Retirement == nil {
			return out, causal.ErrConflict
		}
		return out, s.Store.WitnessColdRetirement(r.Cold.Operation, r.Cold.Source, *r.Retirement)

	}
	return out, causal.ErrConflict
}
