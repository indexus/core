package mesh

import (
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/replication/convergent"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"slices"
)

// replicas is R(z) on a routing view (docs/design/convergent-replication.md
// §3): the holders of the active vnodes closest to zone by XOR, at most one
// per machine, the owner first. machine names a holder's machine; an active
// vnode without a known placement is left out. Like Route, a hint: it grants
// nothing.
func replicas(routing storage.RoutingSnapshot, zone tree.ZoneKey, machine func(causal.Endpoint) string) []causal.Endpoint {
	view := routing.Current()
	holders := make(map[tree.VNodeID]causal.Endpoint, len(view.Active))
	active := make([]tree.VNodeDescriptor, 0, len(view.Active))
	for _, d := range view.Active {
		if h, err := routing.Placement(d.Name); err == nil {
			holders[d.ID] = h
			active = append(active, d)
		}
	}
	ids := convergent.Replicas(zone.ID, active, func(id tree.VNodeID) string { return machine(holders[id]) }, convergent.ReplicaCount)
	out := make([]causal.Endpoint, len(ids))
	for i, id := range ids {
		out[i] = holders[id]
	}
	return out
}

// replicaPeers excludes the actual writer's failure domain even when a
// granular transfer puts the installed writer away from its nominal XOR owner.
func (s *Service) replicaPeers(routing storage.RoutingSnapshot, zone tree.ZoneKey) []causal.Endpoint {
	machine := s.ReplicaMachine
	if machine == nil {
		machine = machineOf
	}
	source := machine(s.Self)
	if source == "" {
		return nil
	}
	return slices.DeleteFunc(replicas(routing, zone, machine), func(member causal.Endpoint) bool {
		domain := machine(member)
		return member.Owner == s.Self.Owner || domain == "" || domain == source
	})
}
