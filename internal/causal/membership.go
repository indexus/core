package causal

import (
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// Member is a routing announcement, never a write permission. Its immutable
// vnode identity reserves its name and address even after retirement. Endpoint
// is a max register; retirement is an irreversible tombstone with one successor.
// Honest local publishers must close admission, transfer every actual right and
// read referral to a live successor, then announce retirement.
type Member struct {
	VNode    tree.VNodeDescriptor
	Endpoint Endpoint
	Retired  bool     `json:",omitempty"`
	Redirect Endpoint `json:",omitzero"`
}

type MembershipDelta struct{ Members []Member }

// Membership retains one record per identity, not the history of its handoffs.
// Convergence applies to compatible announcements from honest publishers, not
// arbitrary forks. Concurrent cyclic retirements are invalid: their rejection
// may depend on arrival order and is not a general CRDT conflict resolution.
// Retired identities are intentionally retained: reclaiming tombstones needs a
// separate agreement about which old participants can ever return.
type Membership struct {
	mu      sync.RWMutex
	root    tree.VNodeDescriptor
	genesis Genesis
	members map[string]Member
	view    *tree.View
	digest  Digest
}

func NewMembership(root tree.VNodeDescriptor, genesis Genesis) (*Membership, error) {
	if root.Name != genesis.Holder.VNode || !genesis.Holder.Valid() || genesis.Cluster == "" {
		return nil, ErrConflict
	}
	m := &Membership{root: root, genesis: genesis, members: map[string]Member{}}
	err := m.Merge(MembershipDelta{Members: []Member{{VNode: root, Endpoint: genesis.Holder}}})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Membership) Clone() *Membership {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return &Membership{root: m.root, genesis: m.genesis, members: maps.Clone(m.members), view: m.view.Clone(), digest: m.digest}
}

func validMember(r Member, root string) bool {
	return r.VNode.Name != "" && !r.VNode.ID.IsZero() && r.Endpoint.Valid() && r.Endpoint.VNode == r.VNode.Name &&
		((!r.Retired && r.Redirect == (Endpoint{})) || (r.Retired && r.VNode.Name != root && r.Redirect.Valid() && r.Redirect.VNode != r.VNode.Name))
}

// Merge validates the entire delta before publishing it. Endpoint conflicts at
// the same retained epoch are refused; older endpoint history is not retained
// for fork detection. The protocol assumes honest, durable local publishers.
func (m *Membership) Merge(delta MembershipDelta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	draft := maps.Clone(m.members)
	ids := make(map[tree.ID]string, len(draft))
	for name, r := range draft {
		ids[r.VNode.ID] = name
	}
	for _, r := range delta.Members {
		if !validMember(r, m.root.Name) {
			return ErrConflict
		}
		if name, ok := ids[r.VNode.ID]; ok && name != r.VNode.Name {
			return ErrConflict
		}
		if old, ok := draft[r.VNode.Name]; ok {
			if old.VNode != r.VNode || old.Endpoint.Epoch == r.Endpoint.Epoch && old.Endpoint != r.Endpoint {
				return ErrConflict
			}
			if old.Retired && r.Retired && old.Redirect != r.Redirect {
				return ErrConflict
			}
			if old.Endpoint.Epoch > r.Endpoint.Epoch {
				r.Endpoint = old.Endpoint
			}
			if old.Retired {
				r.Retired, r.Redirect = true, old.Redirect
			}
		}
		ids[r.VNode.ID] = r.VNode.Name
		draft[r.VNode.Name] = r
	}
	// Redirects can arrive before their target. Known retirement cycles, however,
	// can never resolve and are rejected without publishing a partial update.
	done := make(map[string]bool, len(draft))
	for name := range draft {
		path := map[string]bool{}
		for !done[name] {
			if path[name] {
				return ErrConflict
			}
			path[name] = true
			r, ok := draft[name]
			if !ok || !r.Retired {
				break
			}
			name = r.Redirect.VNode
		}
		for name := range path {
			done[name] = true
		}
	}
	records := memberRecords(draft)
	active := make([]tree.VNodeDescriptor, 0, len(records))
	for _, r := range records {
		if !r.Retired {
			active = append(active, r.VNode)
		}
	}
	view, err := tree.NewViewFrom(0, active)
	if err != nil {
		return ErrConflict
	}
	for _, record := range records {
		if record.Retired {
			view.ReserveRetired(record.VNode.ID)
		}
	}
	m.members, m.view = draft, view
	m.digest = Hash(struct {
		Domain  string
		Root    tree.VNodeDescriptor
		Genesis Genesis
		Members []Member
	}{"indexus-membership", m.root, m.genesis, records})
	return nil
}

func memberRecords(members map[string]Member) []Member {
	records := make([]Member, 0, len(members))
	for _, r := range members {
		records = append(records, r)
	}
	slices.SortFunc(records, func(a, b Member) int { return strings.Compare(a.VNode.Name, b.VNode.Name) })
	return records
}

// Current returns a detached XOR placement view. Placement expresses a desired
// destination; installed local rights alone determine who may acknowledge writes.
func (m *Membership) Current() *tree.View {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.view.Clone()
}
func (m *Membership) Export() MembershipDelta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return MembershipDelta{Members: memberRecords(m.members)}
}
func (m *Membership) Digest() Digest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.digest
}

// ReceiverDrained is a terminal receipt for an already issued cession, never
// permission to write. Honest MOVE/retirement producers close intake and finish
// every confirmed admission before leaving a disk. An issued source cannot have
// cancelled its admission, so that admission must have installed. Address-only
// updates on the same disk do not imply drainage. A physical return to that
// disk is conservatively handled by retrying its durable ticket namespace.
func (m *Membership) ReceiverDrained(target Endpoint) bool {
	if !target.Valid() {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	member, found := m.members[target.VNode]
	if !found || member.Endpoint.Epoch < target.Epoch {
		return false
	}
	return member.Retired || member.Endpoint.Epoch > target.Epoch && member.Endpoint.Owner != target.Owner
}
func (m *Membership) Endpoint(name string) (Endpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.members[name]
	if !ok {
		return Endpoint{}, ErrDependency
	}
	return m.resolve(r.Endpoint)
}
func (m *Membership) Route(id tree.ID) (Endpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	name, _ := m.view.OwnerName(id)
	r, ok := m.members[name]
	if !ok {
		return Endpoint{}, ErrDependency
	}
	return m.resolve(r.Endpoint)
}

// Resolve preserves a future physical endpoint learned from a data ACK until
// membership catches up. This permits early traffic during a physical MOVE and
// prevents A→B→A from sending the future A back to the still-published B.
func (m *Membership) Resolve(hint Endpoint) (Endpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.resolve(hint)
}
func (m *Membership) resolve(hint Endpoint) (Endpoint, error) {
	if !hint.Valid() {
		return Endpoint{}, ErrConflict
	}
	seen := map[string]bool{}
	for {
		if seen[hint.VNode] {
			return Endpoint{}, ErrConflict
		}
		seen[hint.VNode] = true
		r, ok := m.members[hint.VNode]
		if !ok {
			return hint, nil
		}
		if r.Retired {
			hint = r.Redirect
			continue
		}
		if r.Endpoint.Epoch < hint.Epoch {
			return hint, nil
		}
		if r.Endpoint.Epoch == hint.Epoch && r.Endpoint != hint {
			return Endpoint{}, ErrConflict
		}
		return r.Endpoint, nil
	}
}
