package storage

import (
	"bytes"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type membershipSnapshot struct {
	root       tree.VNodeDescriptor
	genesis    causal.Genesis
	digest     causal.Digest
	membership *causal.Membership
}

func (s *Store) pureMembership(state CausalState) (*causal.Membership, error) {
	for i := range s.membershipSnapshots {
		if c := s.membershipSnapshots[i].Load(); c != nil && c.root == state.Root && c.genesis == state.Genesis && c.digest == state.MembershipDigest {
			return c.membership, nil
		}
	}
	m, e := state.MembershipReplica()
	if e != nil {
		return nil, e
	}
	for i := len(s.membershipSnapshots) - 1; i > 0; i-- {
		s.membershipSnapshots[i].Store(s.membershipSnapshots[i-1].Load())
	}
	s.membershipSnapshots[0].Store(&membershipSnapshot{state.Root, state.Genesis, state.MembershipDigest, m})
	return m, nil
}

type causalSnapshot struct {
	version                                      []byte
	state                                        CausalState
	storageID                                    string
	membership                                   *causal.Membership
	view                                         *tree.View
	membershipDigest, routingDigest, inputDigest causal.Digest
}

// The cache key is the exact durable control version, never a routing epoch.
func (s *Store) cachedCausal(version []byte) *causalSnapshot {
	if version == nil {
		return nil
	}
	for _, c := range []*causalSnapshot{s.causalCache.Load(), s.causalPrevious.Load()} {
		if c != nil && bytes.Equal(c.version, version) {
			return c
		}
	}
	return nil
}

// committedCausal is the cached snapshot of the last commit, or nil when the
// cache does not hold it. Routing asks for it for every zone it resolves, so
// the answer is kept per zonelog seq: while no commit happened, nothing is
// read. The seq is read before and after the control version, so an answer
// is only kept for a seq it was read at.
func (s *Store) committedCausal() *causalSnapshot {
	seq := s.db.Seq()
	if v := s.causalVerified.Load(); v != nil && v.seq == seq && (v.snapshot == s.causalCache.Load() || v.snapshot == s.causalPrevious.Load()) {
		return v.snapshot
	}
	var c *causalSnapshot
	_ = s.peek(func(tx *Tx) error { c = s.cachedCausal(controlVersion(tx)); return nil })
	if c != nil && s.db.Seq() == seq {
		s.causalVerified.Store(&verifiedCausal{seq: seq, snapshot: c})
	}
	return c
}

type verifiedCausal struct {
	seq      uint64
	snapshot *causalSnapshot
}

func (s *Store) causalSnapshot(tx *Tx) (*causalSnapshot, error) {
	if c := s.cachedCausal(controlVersion(tx)); c != nil {
		return c, nil
	}
	s.causalCacheMu.Lock()
	defer s.causalCacheMu.Unlock()
	if c := s.cachedCausal(controlVersion(tx)); c != nil {
		return c, nil
	}
	state, v, e := readCausalControlVersion(tx)
	if e != nil {
		return nil, e
	}
	m, e := s.pureMembership(state)
	if e != nil {
		return nil, e
	}
	view := m.Current()
	c := &causalSnapshot{version: bytes.Clone(v), state: state, storageID: storageID(tx), membership: m, view: view, membershipDigest: causal.Digest(view.Digest), routingDigest: m.Digest(), inputDigest: state.MembershipDigest}
	s.causalPrevious.Store(s.causalCache.Load())
	s.causalCache.Store(c)
	return c, nil
}
