package mesh

import (
	"context"
	"errors"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func (s *Service) Join(ctx context.Context) error {
	s.join.Lock()
	defer s.join.Unlock()
	cold, err := s.Store.ColdStatus()
	if err != nil {
		return err
	}
	if cold.Drain != nil {
		return storage.ErrReceiverClosed
	}
	retry := 10 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A failed regional install may consume the donor's last right, so
		// rights discovery alone cannot find that pending ticket afterwards.
		if err := s.resumeIncoming(ctx); err != nil {
			return err
		}
		m, err := s.Membership()
		if err != nil {
			return err
		}
		view := m.Current()
		active := view.HasName(s.VNode.Name)
		if !active {
			if err = view.Add(s.VNode.Name, s.VNode.ID); err != nil {
				return err
			}
		}
		desired, err := tree.Cell(view, s.VNode.Name)
		if err != nil {
			return err
		}
		rights, err := s.Store.Rights(desired)
		if err != nil {
			return err
		}
		var coverage []tree.Region
		for _, right := range rights {
			coverage = append(coverage, right.Region)
		}
		if tree.CoversRegions(desired, coverage) {
			return s.publishActivation()
		}
		progressed := false
		donors, e := s.discoverRights(ctx, m, desired, coverage)
		if e != nil {
			return e
		}
		attempted := map[causal.Owner]bool{}
		for _, donor := range donors {
			if attempted[donor.endpoint.Owner] {
				continue
			}
			attempted[donor.endpoint.Owner] = true
			right := donor.right
			part, ok := right.Region.Intersect(desired)
			if !ok || tree.CoversRegions(part, coverage) {
				continue
			}
			req := Request{Kind: "handoff", Input: right.ID, Region: part, Target: s.Self, TargetVNode: s.VNode, Subdivide: true, Spawned: s.Spawned && !active}
			_, e = s.joinCall(ctx, donor.endpoint, req)
			if e != nil && !errors.Is(e, ErrHandoffPending) && !errors.Is(e, storage.ErrConflict) && !errors.Is(e, storage.ErrNotOwner) && !errors.Is(e, storage.ErrNotReady) && !errors.Is(e, causal.ErrDependency) && !errors.Is(e, causal.ErrConflict) {
				return e
			}
			progressed = progressed || e == nil
		}
		if progressed {
			retry = 10 * time.Millisecond
			if err = s.publishActivation(); err != nil {
				return err
			}
		} else {
			// Busy/stale proposals are not progress. Bound discovery and proposal
			// traffic while the donor finishes its existing durable decisions.
			retry = min(2*retry, 500*time.Millisecond)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
	}
}
func (s *Service) Rebalance(ctx context.Context) error {
	state, err := s.Store.CausalState()
	if err != nil {
		return err
	}
	if state.Moving != nil {
		return nil
	}
	cold, err := s.Store.ColdStatus()
	if err != nil {
		return err
	}
	if cold.Drain != nil {
		return nil
	}
	for _, tr := range state.Transfers {
		if tr.Cession.Target.Owner == s.Self.Owner && tr.Cession.Source.VNode == s.Self.VNode && tr.Phase != causal.TransferInstalled {
			return nil
		}
	}
	m, err := s.Membership()
	if err != nil {
		return err
	}
	endpoint, err := m.Endpoint(s.VNode.Name)
	if err != nil || endpoint.Owner != s.Self.Owner {
		return nil
	}
	return s.Join(ctx)
}
func (s *Service) publishActivation() error {
	s.activationMu.Lock()
	defer s.activationMu.Unlock()
	routing, err := s.Store.CausalRouting()
	if err != nil {
		return err
	}
	if current, e := routing.Placement(s.VNode.Name); e == nil {
		if current.VNode == s.VNode.Name {
			return nil
		} // physical MOVE publishes only at its end
		return storage.ErrNotOwner // irreversible retired logical identity
	}
	// A physical receiver may not yet know its logical member. Same-vnode
	// incoming authority still cannot publish a physical endpoint through ACT-1.
	state, e := s.Store.CausalState()
	if e != nil {
		return e
	}
	for _, tr := range state.Transfers {
		c := tr.Cession
		if c.Target.Owner == s.Self.Owner && c.Source.VNode == c.Target.VNode && c.Source.Owner != c.Target.Owner {
			return nil
		}
	}
	if _, err = s.Store.PublishActivation(s.VNode, s.Self); err != nil {
		if errors.Is(err, storage.ErrNotReady) {
			return nil
		} // no installed first piece yet
		return err
	}
	if s.Activated != nil {
		s.Activated()
	}
	return nil
}

type discoveredRight struct {
	right    causal.Right
	endpoint causal.Endpoint
}

func (s *Service) discoverRights(ctx context.Context, m *causal.Membership, region tree.Region, coverage []tree.Region) ([]discoveredRight, error) {
	// Inserting a vnode changes a local XOR cell. Probe those former neighbours
	// first, not the whole cluster. Successor exceptions bridge stale placement.
	before := m.Current()
	before.Remove(s.VNode.Name)
	var pending, others []causal.Endpoint
	state, err := s.Store.CausalState()
	if err != nil {
		return nil, err
	}
	for _, tr := range state.Transfers {
		if tr.Cession.Target.Owner == s.Self.Owner && tr.Phase != causal.TransferInstalled {
			pending = append(pending, tr.Cession.Source)
		}
	}
	for _, member := range m.Export().Members {
		if member.Retired || member.Endpoint.Owner == s.Self.Owner {
			continue
		}
		cell, e := tree.Cell(before, member.VNode.Name)
		if _, overlap := cell.Intersect(region); e == nil && overlap {
			pending = append(pending, member.Endpoint)
		} else {
			others = append(others, member.Endpoint)
		}
	}
	pending = append(pending, others...)
	seen := map[causal.Endpoint]bool{}
	var failures []error
	for len(pending) > 0 {
		to := pending[0]
		pending = pending[1:]
		if seen[to] || to.Owner == s.Self.Owner {
			continue
		}
		seen[to] = true
		remote, e := s.joinCall(ctx, to, Request{Kind: KindRights, Region: region})
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures = append(failures, e)
			continue
		}
		endpoint := remote.Endpoint
		if !endpoint.Valid() {
			endpoint = to
		}
		var useful []discoveredRight
		for _, right := range remote.Rights {
			if right.Owner != endpoint.Owner {
				return nil, causal.ErrConflict
			}
			part, overlap := right.Region.Intersect(region)
			if overlap && !tree.CoversRegions(part, coverage) {
				useful = append(useful, discoveredRight{right, endpoint})
			}
		}
		if len(useful) > 0 {
			return useful, nil
		}
		var refs []causal.Endpoint
		for _, ref := range remote.Directory {
			part, overlap := ref.Region.Intersect(region)
			if overlap && !tree.CoversRegions(part, coverage) {
				refs = append(refs, ref.Endpoint)
			}
		}
		pending = append(refs, pending...)
	}
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return nil, nil
}
