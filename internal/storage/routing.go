package storage

import (
	"math"
	"math/bits"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type RoutingSnapshot struct{ snapshot *causalSnapshot }

func (r RoutingSnapshot) Identity() (tree.VNodeDescriptor, causal.Genesis) {
	return r.snapshot.state.Root, r.snapshot.state.Genesis
}
func (r RoutingSnapshot) Current() *tree.View          { return r.snapshot.view.Clone() }
func (r RoutingSnapshot) Digest() causal.Digest        { return r.snapshot.membershipDigest }
func (r RoutingSnapshot) RoutingDigest() causal.Digest { return r.snapshot.routingDigest }
func (r RoutingSnapshot) Placement(name string) (causal.Endpoint, error) {
	return r.snapshot.membership.Endpoint(name)
}
func (r RoutingSnapshot) Route(id tree.ID) (causal.Endpoint, error) {
	return r.snapshot.membership.Route(id)
}
func (r RoutingSnapshot) Resolve(h causal.Endpoint) (causal.Endpoint, error) {
	return r.snapshot.membership.Resolve(h)
}

// Cell reads the immutable placement index without cloning the network view.
// It is an advisory cell, never authority to write.
func (r RoutingSnapshot) Cell(name string) (tree.Region, error) {
	return tree.Cell(r.snapshot.view, name)
}

// WriteRegionHint is a conservative route hint for a successfully ACKed point.
// It never grants permission. Native pieces include empty addresses; point-only
// installations and pending outgoing pieces cannot advertise a whole XOR cell.
func (s *Store) WriteRegionHint(id tree.ID) tree.Region {
	// An ACK must not wait for a cache rebuild solely to improve its hint.
	c := s.committedCausal()
	if c == nil {
		return tree.Point(id)
	}
	right, ok := c.state.Permissions.At(id)
	if !ok {
		return tree.Point(id)
	}
	region := right.Region
	for _, tr := range c.state.Transfers {
		cession := tr.Cession
		if cession.Source.Storage != c.storageID || cession.Input.ID != right.ID || tr.Phase == causal.TransferCancelled {
			continue
		}
		if cession.Region.Contains(id) {
			return tree.Point(id)
		}
		for _, remaining := range region.Subtract(cession.Region) {
			if remaining.Contains(id) {
				region = remaining
				break
			}
		}
	}
	return region
}

// ReceiverDrained is a terminal receipt from an honest endpoint producer,
// never authority to write. Only an already issued source may use it to close.
func (r RoutingSnapshot) ReceiverDrained(target causal.Endpoint) bool {
	return r.snapshot.membership.ReceiverDrained(target)
}

func (r RoutingSnapshot) HasOpenTransfer() bool {
	for _, t := range r.snapshot.state.Transfers {
		if t.Phase != causal.TransferInstalled && t.Phase != causal.TransferCancelled {
			return true
		}
	}
	return false
}
func (s *Store) CausalRouting() (RoutingSnapshot, error) {
	if c := s.committedCausal(); c != nil {
		return RoutingSnapshot{c}, nil
	}
	c, e := read(s, s.causalSnapshot)
	return RoutingSnapshot{c}, e
}

// NativeAddressShare reports the geometry of installed regional rights from the
// immutable cache. Point activations during an unfinished handoff are separate.
func (s *Store) NativeAddressShare() (float64, bool) {
	c := s.committedCausal()
	if c == nil {
		return 0, false
	}
	var share float64
	for _, right := range c.state.Permissions.Native {
		fixed := 0
		for _, mask := range right.Region.Mask {
			fixed += bits.OnesCount8(mask)
		}
		share += math.Ldexp(1, -fixed)
	}
	return share, true
}
