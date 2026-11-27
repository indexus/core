package modelcheck

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// A bounded independent scheduler explores all deliveries and crash boundaries
// for one authority chain D -> J -> K. Real disk cut tests separately check that
// the implementation persists these transitions in the claimed order.
func TestCausalDivergentViewsExplore(t *testing.T) {
	type state struct {
		Phase uint8
		Views [3]uint8
		ACK   bool
		Bytes [3]bool
	}
	root := causal.Genesis{Cluster: "model", Holder: causal.Endpoint{Host: "D", Owner: causal.Owner{VNode: "D", Storage: "disk-D"}}}
	j, k := causal.Endpoint{Host: "J", Owner: causal.Owner{VNode: "J", Storage: "disk-J"}}, causal.Endpoint{Host: "K", Owner: causal.Owner{VNode: "K", Storage: "disk-K"}}
	// These are real pure-kernel transitions; the scheduler below independently
	// explores routing delivery and acknowledged bytes around their disk cuts.
	initial := causal.LocalPermissions{Native: []causal.Right{root.Right()}}
	emitted, receiver, first := permissionHandoff(t, initial, root.Holder, j)
	_, _, second := permissionHandoff(t, receiver, j, k)
	_ = second
	queue := []state{{Views: [3]uint8{1, 1, 1}}}
	seen := map[state]bool{}
	divergent := 0
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		if seen[s] {
			continue
		}
		seen[s] = true
		writers := 0
		writer := -1
		// Phase boundaries: writable, frozen/copied, emitted, installed; repeated
		// once for the second handoff. Install is independent of routing knowledge.
		for node := 0; node < 3; node++ {
			writable := (node == 0 && s.Phase == 0) || (node == 1 && (s.Phase == 3 || s.Phase == 4)) || (node == 2 && s.Phase >= 7)
			if writable {
				writers++
				writer = node
			}
		}
		if writers > 1 {
			t.Fatal("multiple ACK authorities")
		}
		if s.ACK {
			exists := false
			for _, b := range s.Bytes {
				exists = exists || b
			}
			if !exists {
				t.Fatal("acknowledged bytes vanished")
			}
			if writer >= 0 && !s.Bytes[writer] {
				t.Fatal("new authority lacks acknowledged state")
			}
		}
		owners := map[tree.ID]bool{}
		for _, mask := range s.Views {
			view := tree.NewView(0)
			for i, name := range []string{"D", "J", "K"} {
				if mask&(1<<i) != 0 {
					_ = view.Add(name, tree.ID{byte(i*80 + 1)})
				}
			}
			owners[view.Owner(tree.ID{170})] = true
		}
		if len(owners) > 1 {
			divergent++
		}
		// Network deliveries can be delayed, duplicated or reordered independently.
		for replica := 0; replica < 3; replica++ {
			for bit := uint8(2); bit <= 4; bit <<= 1 {
				if (bit == 2 && s.Phase < 3) || (bit == 4 && s.Phase < 7) {
					continue
				}
				next := s
				next.Views[replica] |= bit
				queue = append(queue, next)
			}
		}
		// A crash restores the same durable phase, not a new writable incarnation.
		queue = append(queue, s)
		if writer >= 0 && !s.ACK {
			next := s
			next.ACK = true
			next.Bytes[writer] = true
			queue = append(queue, next)
		}
		if s.Phase < 8 {
			next := s
			next.Phase++
			if s.Phase == 0 {
				next.Bytes[1] = s.Bytes[0]
			}
			if s.Phase == 4 {
				next.Bytes[2] = s.Bytes[1]
			}
			if s.Phase == 3 {
				next.Bytes[0] = false
			}
			if s.Phase == 7 {
				next.Bytes[1] = false
			}
			queue = append(queue, next)
		}
		// Emitting a competing successor never succeeds, including after delivery
		// of only a subset of the routing activations.
		if s.Phase >= 2 {
			fork := first
			fork.Cession.DataDigest = causal.Hash("competing-seal")
			fork.Phase = causal.TransferSealed
			if _, _, err := emitted.Emit(fork, root.Holder.Owner); err == nil {
				t.Fatal("spent local input accepted a competing cession")
			}
		}
	}
	if len(seen) < 100 || divergent == 0 {
		t.Fatal("scheduler did not explore divergent views")
	}
	t.Log(fmt.Sprintf("%d states, %d with divergent routing owners", len(seen), divergent))
}

// permissionHandoff executes reservation, confirmed preload metadata, seal,
// irreversible source cession and receiver installation. Bytes are abstracted
// separately above; this is not a refinement proof of storage transactions.
func permissionHandoff(t *testing.T, p causal.LocalPermissions, source, target causal.Endpoint) (causal.LocalPermissions, causal.LocalPermissions, causal.PermissionTransfer) {
	t.Helper()
	var err error
	p, outgoing, err := p.Begin("model", p.Native[0].ID, tree.Region{}, source, target)
	if err != nil {
		t.Fatal(err)
	}
	receiver := causal.LocalPermissions{}
	receiver, slot, _, err := receiver.Reserve(outgoing.Cession, nil, target.Owner)
	if err != nil {
		t.Fatal(err)
	}
	outgoing, err = causal.BindTicket(outgoing, slot.Cession.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	slot, err = causal.Confirm(slot, outgoing.Cession)
	if err != nil {
		t.Fatal(err)
	}
	outgoing, err = causal.Confirm(outgoing, outgoing.Cession)
	if err != nil {
		t.Fatal(err)
	}
	seal := outgoing.Cession
	seal.DataDigest = causal.Hash("certified-model-data")
	outgoing, err = causal.Seal(outgoing, seal)
	if err != nil {
		t.Fatal(err)
	}
	slot, err = causal.Seal(slot, seal)
	if err != nil {
		t.Fatal(err)
	}
	p, outgoing, err = p.Emit(outgoing, source.Owner)
	if err != nil {
		t.Fatal(err)
	}
	receiver, _, _, err = receiver.Install(seal, &slot, target.Owner)
	if err != nil {
		t.Fatal(err)
	}
	return p, receiver, outgoing
}
