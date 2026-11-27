package mesh

import (
	"context"
	"errors"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
)

func (s *Service) membershipTransport() *membershipWire {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	if s.membership == nil {
		s.membership = newMembershipWire(func() (*MembershipEnvelope, error) {
			root, genesis, delta, err := s.Store.Membership()
			if err != nil {
				return nil, err
			}
			return &MembershipEnvelope{Root: root, Genesis: genesis, Delta: delta}, nil
		}, func(e MembershipEnvelope) error {
			root, genesis, _, err := s.Store.Membership()
			if err != nil {
				return err
			}
			if e.Root != root || e.Genesis != genesis {
				return storage.ErrWrongCluster
			}
			return s.Store.MergeMembership(e.Delta)
		})
		if s.controlClosed {
			s.membership.close()
		}
	}
	return s.membership
}
func (s *Service) Membership() (*causal.Membership, error) {
	root, genesis, delta, err := s.Store.Membership()
	if err != nil {
		return nil, err
	}
	m, err := causal.NewMembership(root, genesis)
	if err != nil {
		return nil, err
	}
	err = m.Merge(delta)
	return m, err
}
func (s *Service) membershipSend(ctx context.Context, host string, p MembershipPacket) (MembershipReply, error) {
	out, err := s.call(ctx, host, Request{Kind: KindMembership, Membership: &p, Owner: s.Self})
	if err != nil {
		return MembershipReply{}, err
	}
	if out.Membership == nil {
		return MembershipReply{}, causal.ErrDependency
	}
	return *out.Membership, nil
}
func (s *Service) Sync(ctx context.Context, hosts []string) error {
	var failures []error
	for _, host := range hosts {
		if host != s.Self.Host {
			if err := s.membershipTransport().exchange(ctx, host, s.membershipSend); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
