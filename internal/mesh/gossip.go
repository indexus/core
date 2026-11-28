package mesh

import (
	"encoding/binary"
	"sort"

	"github.com/indexus/go-indexus-core/internal/metrics"
)

// GossipTargets samples current routing peers plus active transfer contacts.
// No authority history or former-certificate graph is exported.
func (s *Service) GossipTargets(round uint64) ([]string, error) {
	r, err := s.Store.CausalRouting()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{s.Self.Host: true}
	var peers []string
	add := func(host string) {
		if !seen[host] {
			seen[host] = true
			peers = append(peers, host)
		}
	}
	offset := round + binary.BigEndian.Uint64(s.VNode.ID[:8])
	known, err := r.VisitBucketHosts(s.VNode.ID, offset, add)
	if err != nil {
		return nil, err
	}
	metrics.Gossip.KnownVNodes.Store(int64(known))
	hosts := r.TransferHosts()
	rest := hosts[:0]
	for _, host := range hosts {
		if !seen[host] {
			rest = append(rest, host)
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		add(rest[offset%uint64(len(rest))])
	}
	return peers, nil
}
