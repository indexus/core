package storage

import (
	"errors"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestColdLegacyDrainRetainsItsRecordedGuard(t *testing.T) {
	for _, changed := range []bool{false, true} {
		_, donor, g := ticketStores(t)
		from, err := donor.Endpoint("B")
		if err != nil {
			t.Fatal(err)
		}
		local := causal.Member{VNode: tree.VNodeDescriptor{Name: "B", ID: tree.ID{2}}, Endpoint: from}
		if err = donor.MergeMembership(causal.MembershipDelta{Members: []causal.Member{local}}); err != nil {
			t.Fatal(err)
		}
		r, err := donor.CausalRouting()
		if err != nil {
			t.Fatal(err)
		}
		// Persist the fields available to a process interrupted after the old pin.
		err = donor.update(func(tx *Tx) error {
			return tx.putJSON(meta(coldDrainKey), map[string]any{
				"Operation": "old-drain", "Source": from, "Target": g.Holder, "BaseDigest": r.ViewDigest(), "PinConfirmed": true,
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			far := causal.Member{VNode: tree.VNodeDescriptor{Name: "C", ID: tree.ID{128}}, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: "C", Storage: "third"}, Host: "third"}}
			if err = donor.MergeMembership(causal.MembershipDelta{Members: []causal.Member{far}}); err != nil {
				t.Fatal(err)
			}
		}
		_, err = donor.PublishColdRetirement()
		if (!changed && err != nil) || (changed && !errors.Is(err, protocol.ErrColdPlanPreempted)) {
			t.Fatalf("changed=%t retirement=%v", changed, err)
		}
	}
}

// All pairs plan against the same 64-vnode view. Retire a pair and deliver
// that announcement to every remaining planner before the next retirement.
func TestIndependentColdPairsSurviveConcurrentRetirements(t *testing.T) {
	var members []causal.Member
	for i := 0; i < 64; i++ {
		name := fmt.Sprintf("v%02d", i)
		members = append(members, causal.Member{VNode: tree.VNodeDescriptor{Name: name, ID: tree.ID{1, byte(i)}}, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: name + "-disk"}, Host: name}})
	}
	m, err := causal.NewMembership(members[0].VNode, causal.Genesis{Cluster: "cold-wave", Holder: members[0].Endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Merge(causal.MembershipDelta{Members: members}); err != nil {
		t.Fatal(err)
	}
	view := m.Current()
	original := view.Digest
	plans := make([]ColdDrain, 0, 32)
	for i := 1; i < len(members); i += 2 {
		cell, e := tree.Cell(view, members[i].VNode.Name)
		if e != nil {
			t.Fatal(e)
		}
		plans = append(plans, ColdDrain{Source: members[i].Endpoint, Target: members[i-1].Endpoint, SourceCell: &cell})
	}
	for i, plan := range plans {
		if !plan.matchesPair(view, m) {
			t.Fatalf("disjoint retirement invalidated pair %d", i)
		}
		if i > 0 && original == view.Digest {
			t.Fatal("expected new membership generation")
		}
		retired := members[2*i+1]
		retired.Retired = true
		retired.Redirect = plan.Target
		if err := m.Merge(causal.MembershipDelta{Members: []causal.Member{retired}}); err != nil {
			t.Fatal(err)
		}
		view = m.Current()
	}
	// A receiver endpoint change alone must invalidate its old pair, even
	// when the active identities and their cells stay exactly the same.
	survivor := members[0]
	source := members[2]
	cell, err := tree.Cell(view, source.VNode.Name)
	if err != nil {
		t.Fatal(err)
	}
	plan := ColdDrain{Source: source.Endpoint, Target: survivor.Endpoint, SourceCell: &cell}
	if !plan.matchesPair(view, m) {
		t.Fatal("fixture does not select the survivor")
	}
	survivor.Endpoint.Epoch++
	survivor.Endpoint.Host = "new-host"
	if err = m.Merge(causal.MembershipDelta{Members: []causal.Member{survivor}}); err != nil {
		t.Fatal(err)
	}
	if plan.matchesPair(m.Current(), m) {
		t.Fatal("receiver endpoint changed")
	}
}
