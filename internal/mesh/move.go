package mesh

import (
	"context"
	"errors"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func (s *Service) MoveTo(ctx context.Context, target causal.Endpoint) error {
	s.move.Lock()
	defer s.move.Unlock()
	state, err := s.Store.CausalState()
	if err != nil {
		return err
	}
	if state.Moving != nil {
		target = *state.Moving
	} else {
		m, e := state.MembershipReplica()
		if e != nil {
			return e
		}
		current, e := m.Endpoint(s.VNode.Name)
		if e != nil {
			return e
		}
		if current.Owner == target.Owner {
			return nil
		}
		if current.Epoch == ^uint64(0) {
			return causal.ErrConflict
		}
		target.Epoch = current.Epoch + 1
		if err = s.Store.BeginCausalMove(target); err != nil {
			return err
		}
	}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		rights, e := s.Store.Rights(tree.Region{})
		if e != nil {
			return e
		}
		if len(rights) == 0 {
			break
		}
		right := rights[0]
		err = s.handoff(ctx, Request{Kind: "handoff", Input: right.ID, Target: target, Region: right.Region, Subdivide: true})
		if errors.Is(err, storage.ErrConflict) || errors.Is(err, causal.ErrConflict) || errors.Is(err, storage.ErrNotOwner) {
			continue
		}
		if err != nil {
			return err
		}
	}
	directory, e := s.Store.ReadDirectory()
	if e != nil {
		return e
	}
	if _, err = s.call(ctx, target.Host, Request{Kind: "directory", Directory: directory}); err != nil {
		return err
	}
	if _, err = s.Store.PublishCausalMove(); err != nil {
		return err
	}
	return s.Sync(ctx, []string{target.Host})
}
func (s *Service) ReceiveMove(ctx context.Context, source string) error {
	_, err := s.call(ctx, source, Request{Kind: "move-host", Target: s.Self})
	return err
}
