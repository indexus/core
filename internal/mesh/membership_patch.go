package mesh

import "github.com/indexus/go-indexus-core/internal/causal"

// These deltas describe transitions between canonical, complete envelopes.
// A transition may add an identity or advance its record. Retired identities
// remain present. The receiver verifies the reconstructed digest and passes
// records through the usual causal merger before acknowledging the version.
func sameMembershipCluster(a, b *MembershipEnvelope) bool {
	return a != nil && b != nil && a.Root == b.Root && a.Genesis == b.Genesis
}

func advancesMember(old, next causal.Member) bool {
	if old.VNode != next.VNode || next.Endpoint.Epoch < old.Endpoint.Epoch {
		return false
	}
	if next.Endpoint.Epoch == old.Endpoint.Epoch && next.Endpoint != old.Endpoint {
		return false
	}
	return !old.Retired || (next.Retired && next.Redirect == old.Redirect)
}

// membershipChanges walks the two sorted versions together. Only differing
// records are allocated; nil requests the full-envelope path.
func membershipChanges(previous, next *MembershipEnvelope) *MembershipEnvelope {
	if !sameMembershipCluster(previous, next) {
		return nil
	}
	base := previous.Delta.Members
	var changes []causal.Member
	var last string
	for _, member := range next.Delta.Members {
		name := member.VNode.Name
		if name <= last {
			return nil
		}
		last = name
		if len(base) != 0 {
			old := base[0]
			if old.VNode.Name < name {
				return nil // removed identity, or noncanonical base
			}
			if old.VNode.Name == name {
				base = base[1:]
				if old == member {
					continue
				}
				if !advancesMember(old, member) {
					return nil
				}
			}
		}
		changes = append(changes, member)
	}
	if len(base) != 0 || len(changes) == 0 || len(changes) == len(next.Delta.Members) {
		return nil
	}
	return &MembershipEnvelope{Root: next.Root, Genesis: next.Genesis, Delta: causal.MembershipDelta{Members: changes}}
}

// expandMembership reconstructs name order directly, without sorting or
// allocating a map. Neither the retained base nor a packet's slice is mutated.
func expandMembership(base, patch *MembershipEnvelope) (*MembershipEnvelope, error) {
	if !sameMembershipCluster(base, patch) || len(patch.Delta.Members) == 0 {
		return nil, causal.ErrConflict
	}
	old, changed := base.Delta.Members, patch.Delta.Members
	members := make([]causal.Member, 0, len(old)+len(changed))
	var last string
	for len(old) > 0 || len(changed) > 0 {
		var member causal.Member
		if len(changed) == 0 || (len(old) > 0 && old[0].VNode.Name < changed[0].VNode.Name) {
			member, old = old[0], old[1:]
		} else {
			member, changed = changed[0], changed[1:]
			if len(old) > 0 && old[0].VNode.Name == member.VNode.Name {
				if old[0] == member || !advancesMember(old[0], member) {
					return nil, causal.ErrConflict
				}
				old = old[1:]
			}
		}
		if member.VNode.Name <= last {
			return nil, causal.ErrConflict
		}
		last = member.VNode.Name
		members = append(members, member)
	}
	return &MembershipEnvelope{Root: base.Root, Genesis: base.Genesis, Delta: causal.MembershipDelta{Members: members}}, nil
}
