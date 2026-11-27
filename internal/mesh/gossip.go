package mesh

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// GossipMember is one current holder as this replica sees it.
type GossipMember struct {
	ID   tree.ID
	Host string
}

// GossipPeers picks one round of anti-entropy partners. Knowing every vnode is
// cheap (the routing map); talking to every host each round is what costs N².
//
// Members are grouped into Kademlia buckets by the length of their common
// prefix with self. The round contacts one member per non-empty bucket and
// rotates within it, so every member is reached within |bucket| rounds. One
// former host (still forwarding for stale peers) is added per round, rotating.
//
// GOS-1: in every round, for every target t, the contacted member of t's
// bucket shares a strictly longer prefix with t. A fact therefore crosses the
// whole network in at most depth(vnode trie) rounds of successful exchanges,
// whichever member each bucket picks. See docs/protocol/gossip.md.
func GossipPeers(self tree.ID, selfHost string, members []GossipMember, former []string, round uint64) []string {
	var buckets [tree.Size*8 + 1][]GossipMember
	for _, m := range members {
		if m.ID == self || m.Host == "" {
			continue
		}
		cpl := tree.CommonPrefixLen(self, m.ID)
		buckets[cpl] = append(buckets[cpl], m)
	}
	// Decorrelate nodes so a bucket's members are not all picked in lockstep.
	salt := binary.BigEndian.Uint64(self[:8])
	seen := map[string]bool{selfHost: true}
	var out []string
	add := func(host string) {
		if !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	for i, bucket := range buckets {
		if len(bucket) == 0 {
			continue
		}
		sort.Slice(bucket, func(a, b int) bool { return bytes.Compare(bucket[a].ID[:], bucket[b].ID[:]) < 0 })
		add(bucket[(round+salt+uint64(i))%uint64(len(bucket))].Host)
	}
	var rest []string
	for _, host := range former {
		if !seen[host] {
			rest = append(rest, host)
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		add(rest[(round+salt)%uint64(len(rest))])
	}
	return out
}

// GossipTargets samples current routing peers plus active transfer contacts.
// No authority history or former-certificate graph is exported.
func (s *Service) GossipTargets(round uint64) ([]string, error) {
	m, err := s.Membership()
	if err != nil {
		return nil, err
	}
	var members []GossipMember
	for _, member := range m.Export().Members {
		if !member.Retired {
			members = append(members, GossipMember{ID: member.VNode.ID, Host: member.Endpoint.Host})
		}
	}
	state, err := s.Store.CausalState()
	if err != nil {
		return nil, err
	}
	var contacts []string
	for _, tr := range state.Transfers {
		contacts = append(contacts, tr.Cession.Source.Host, tr.Cession.Target.Host)
	}
	metrics.Gossip.KnownVNodes.Store(int64(len(members)))
	return GossipPeers(s.VNode.ID, s.Self.Host, members, contacts, round), nil
}
