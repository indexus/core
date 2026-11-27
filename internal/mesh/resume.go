package mesh

import (
	"context"
	"errors"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// Resume carries every durable decision of this disk one step further: an
// unwitnessed cold drain first, then each transfer by its phase, then the
// local housekeeping and the incoming transfers. Each part is independent:
// the failure of one never suppresses the others.
func (s *Service) Resume(ctx context.Context) error {
	cold, err := s.Store.ColdStatus()
	if err != nil {
		return err
	}
	if cold.Drain != nil && !cold.Drain.Witnessed {
		return s.ResumeCold(ctx)
	}
	state, err := s.Store.CausalState()
	if err != nil {
		return err
	}
	var failures []error
	// Finish irreversible outgoing work first. A warm attempt can spend its
	// entire admission interval waiting for another vnode on this host.
	for _, urgent := range []bool{true, false} {
		for id, tr := range state.Transfers {
			needsFinish := tr.Cession.Source.Owner == s.Self.Owner && tr.Phase >= causal.TransferMoving && tr.Phase < causal.TransferInstalled
			if needsFinish == urgent {
				failures = append(failures, s.resumeTransfer(ctx, id, tr))
			}
		}
	}
	failures = append(failures, s.resumeLocal(ctx, state)...)
	// An unavailable incoming source must never suppress an independent
	// outgoing durable decision, collection, or local frontier compaction.
	failures = append(failures, s.resumeIncoming(ctx))
	return errors.Join(failures...)
}

// resumeTransfer advances one transfer: a cancelled one is cancelled on both
// sides, an installed one is collected, any other is pursued by its source.
func (s *Service) resumeTransfer(ctx context.Context, id causal.TransferID, tr storage.CausalTransfer) error {
	c := tr.Cession
	source := c.Source.Owner == s.Self.Owner
	var err error
	switch {
	case tr.Phase == causal.TransferCancelled && c.Target.Owner == s.Self.Owner:
		return s.Store.CancelCausal(c)
	case tr.Phase == causal.TransferCancelled:
		err = s.finishCancellation(ctx, c)
	case tr.Phase == causal.TransferInstalled:
		err = s.Store.CollectTransferred(id)
	case !source:
		return nil
	default:
		return s.pursueTransfer(ctx, id, c)
	}
	if err == nil && source {
		metrics.HandoffEnded(id.String(), true)
	}
	return err
}

// pursueTransfer runs the handoff of a transfer this disk is the source of.
// A conflict on a transfer that no longer exists is not a failure.
func (s *Service) pursueTransfer(ctx context.Context, id causal.TransferID, c causal.Cession) error {
	err := s.handoff(ctx, Request{Kind: "handoff", Operation: id, Input: c.Input.ID, Target: c.Target, Region: c.Region})
	if !errors.Is(err, storage.ErrConflict) {
		return err
	}
	latest, readErr := s.Store.CausalState()
	if readErr != nil {
		return err
	}
	if _, exists := latest.Transfers[id]; !exists {
		return nil
	}
	return err
}

// resumeLocal is the housekeeping that needs no other disk: a host move in
// progress, otherwise merging local rights and compacting the read directory.
func (s *Service) resumeLocal(ctx context.Context, state storage.CausalState) (failures []error) {
	if state.Moving != nil {
		return []error{s.MoveTo(ctx, *state.Moving)}
	}
	for _, step := range []func() error{s.Store.CoalesceLocalRights, s.Store.CompactReadDirectory} {
		if err := step(); err != nil && !errors.Is(err, storage.ErrNotReady) {
			failures = append(failures, err)
		}
	}
	return failures
}

func (s *Service) resumeIncoming(ctx context.Context) error {
	state, err := s.Store.CausalState()
	if err != nil {
		return err
	}
	var failures []error
	for id, tr := range state.Transfers {
		c := tr.Cession
		if c.Target.Owner != s.Self.Owner || tr.Phase == causal.TransferInstalled || tr.Phase == causal.TransferCancelled {
			continue
		}
		_, e := s.joinCall(ctx, c.Source, Request{Kind: "handoff", Operation: id, Input: c.Input.ID, Target: c.Target, Region: c.Region})
		if e != nil && !errors.Is(e, ErrHandoffPending) {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}
func (s *Service) finishCancellation(ctx context.Context, c causal.Cession) error {
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return err
	}
	if c.Ticket != 0 && !routing.ReceiverDrained(c.Target) {
		if _, err = s.endpointCall(ctx, c.Target, Request{Kind: "cancel", Cession: &c}); err != nil {
			return err
		}
	}
	return s.Store.FinishAbortCausal(c.ID)
}
