package causal

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func membershipFixture(t *testing.T) (*Membership, Member, Member) {
	t.Helper()
	root := tree.VNodeDescriptor{Name: "root", ID: tree.ID{1}}
	m, err := NewMembership(root, Genesis{Cluster: "test", Holder: Endpoint{Host: "root:1", Owner: Owner{VNode: "root", Storage: "disk-root"}}})
	if err != nil {
		t.Fatal(err)
	}
	a := Member{VNode: tree.VNodeDescriptor{Name: "a", ID: tree.ID{128}}, Endpoint: Endpoint{Owner: Owner{VNode: "a", Storage: "disk-a"}, Host: "a:1"}}
	b := Member{VNode: tree.VNodeDescriptor{Name: "b", ID: tree.ID{64}}, Endpoint: Endpoint{Owner: Owner{VNode: "b", Storage: "disk-b"}, Host: "b:1"}}
	return m, a, b
}

func TestMembershipConvergesWithoutHandoffHistory(t *testing.T) {
	base, a, b := membershipFixture(t)
	moved := a
	moved.Endpoint = Endpoint{Owner: Owner{VNode: "a", Storage: "disk-a2"}, Host: "a:2", Epoch: 1}
	returned := a
	returned.Endpoint.Epoch = 2
	retired := returned
	retired.Retired = true
	retired.Redirect = b.Endpoint
	records := []Member{a, b, moved, returned, retired}
	var expected Digest
	rng := rand.New(rand.NewSource(20261125))
	for run := 0; run < 100; run++ {
		m := base.Clone()
		for _, i := range rng.Perm(len(records)) {
			delta := MembershipDelta{Members: []Member{records[i]}}
			if err := m.Merge(delta); err != nil {
				t.Fatal(err)
			}
			before := m.Digest()
			if err := m.Merge(delta); err != nil {
				t.Fatal(err)
			}
			if m.Digest() != before {
				t.Fatal("duplicate changed state")
			}
		}
		if run == 0 {
			expected = m.Digest()
		}
		if m.Digest() != expected {
			t.Fatal("delivery order changed final state")
		}
		if len(m.Export().Members) != 3 {
			t.Fatal("retained handoff history")
		}
		if len(m.Current().Active) != 2 {
			t.Fatal("retired member is active")
		}
		if !m.Current().IsRetiredID(a.VNode.ID) {
			t.Fatal("routing view forgot the retired identity namespace")
		}
		if got, err := m.Endpoint("a"); err != nil || got != b.Endpoint {
			t.Fatalf("retirement not followed: %+v %v", got, err)
		}
	}
}

func TestMembershipFutureEndpointAndPhysicalReturn(t *testing.T) {
	m, a, _ := membershipFixture(t)
	if err := m.Merge(MembershipDelta{Members: []Member{a}}); err != nil {
		t.Fatal(err)
	}
	moved := a
	moved.Endpoint.Storage = "disk-second"
	moved.Endpoint.Host = "a:2"
	moved.Endpoint.Epoch = 1
	if err := m.Merge(MembershipDelta{Members: []Member{moved}}); err != nil {
		t.Fatal(err)
	}
	future := a.Endpoint
	future.Epoch = 2
	if got, err := m.Resolve(future); err != nil || got != future {
		t.Fatalf("future return redirected before publication: %+v %v", got, err)
	}
	if got, err := m.Resolve(a.Endpoint); err != nil || got != moved.Endpoint {
		t.Fatalf("old route not advanced: %+v %v", got, err)
	}
	returned := a
	returned.Endpoint = future
	if err := m.Merge(MembershipDelta{Members: []Member{returned}}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Resolve(moved.Endpoint); err != nil || got != future {
		t.Fatalf("old physical endpoint not advanced: %+v %v", got, err)
	}
	// A receiver can be contacted before its first activation is gossiped.
	unknown := Endpoint{Owner: Owner{VNode: "new", Storage: "fresh"}, Host: "new:1", Epoch: 4}
	if got, err := m.Resolve(unknown); err != nil || got != unknown {
		t.Fatal(got, err)
	}
}

func TestMembershipTerminalReceiptRequiresActualDiskDeparture(t *testing.T) {
	m, a, b := membershipFixture(t)
	if err := m.Merge(MembershipDelta{Members: []Member{a, b}}); err != nil {
		t.Fatal(err)
	}
	if m.ReceiverDrained(a.Endpoint) {
		t.Fatal("current address is not a terminal receipt")
	}
	addressOnly := a
	addressOnly.Endpoint.Host, addressOnly.Endpoint.Epoch = "a:new-address", 1
	if err := m.Merge(MembershipDelta{Members: []Member{addressOnly}}); err != nil {
		t.Fatal(err)
	}
	if m.ReceiverDrained(a.Endpoint) {
		t.Fatal("address-only restart falsely certified installation")
	}
	moved := addressOnly
	moved.Endpoint.Storage, moved.Endpoint.Host, moved.Endpoint.Epoch = "new-disk", "a:new-disk", 2
	if err := m.Merge(MembershipDelta{Members: []Member{moved}}); err != nil {
		t.Fatal(err)
	}
	if !m.ReceiverDrained(a.Endpoint) || m.ReceiverDrained(moved.Endpoint) {
		t.Fatal("physical departure did not distinguish old and current admissions")
	}
	returned := a
	returned.Endpoint.Epoch = 3
	if err := m.Merge(MembershipDelta{Members: []Member{returned}}); err != nil {
		t.Fatal(err)
	}
	if m.ReceiverDrained(a.Endpoint) {
		t.Fatal("return must retry the original disk's ticket namespace")
	}
	returned.Retired, returned.Redirect = true, b.Endpoint
	if err := m.Merge(MembershipDelta{Members: []Member{returned}}); err != nil {
		t.Fatal(err)
	}
	future := a.Endpoint
	future.Epoch = 4
	if !m.ReceiverDrained(a.Endpoint) || m.ReceiverDrained(future) || m.ReceiverDrained(Endpoint{}) {
		t.Fatal("retirement receipt ignored its endpoint epoch")
	}
}

func TestMembershipRejectsConflictsAtomically(t *testing.T) {
	m, a, b := membershipFixture(t)
	if err := m.Merge(MembershipDelta{Members: []Member{a, b}}); err != nil {
		t.Fatal(err)
	}
	fork := a
	fork.Endpoint.Host = "fork:1"
	renamed := a
	renamed.VNode.Name = "alias"
	renamed.Endpoint.VNode = "alias"
	newID := a
	newID.VNode.ID = tree.ID{129}
	retiredRoot := m.Export().Members[2]
	retiredRoot.Retired = true
	retiredRoot.Redirect = a.Endpoint
	badRedirect := a
	badRedirect.Retired = true
	badRedirect.Redirect = a.Endpoint
	cycleA := a
	cycleA.Retired = true
	cycleA.Redirect = b.Endpoint
	cycleB := b
	cycleB.Retired = true
	cycleB.Redirect = a.Endpoint
	for _, bad := range []MembershipDelta{
		{Members: []Member{fork}}, {Members: []Member{renamed}}, {Members: []Member{newID}},
		{Members: []Member{retiredRoot}}, {Members: []Member{badRedirect}},
		{Members: []Member{cycleA, cycleB}},
	} {
		before := m.Export()
		digest := m.Digest()
		// The valid update preceding the invalid record must not leak out either.
		update := b
		update.Endpoint.Epoch = 1
		update.Endpoint.Host = "new:2"
		bad.Members = append([]Member{update}, bad.Members...)
		if err := m.Merge(bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("accepted conflict %+v: %v", bad, err)
		}
		if m.Digest() != digest || !reflect.DeepEqual(m.Export(), before) {
			t.Fatal("failed merge mutated membership")
		}
	}
}

func TestMembershipRetirementBeforeActivationAndRedirectMove(t *testing.T) {
	m, a, b := membershipFixture(t)
	retired := a
	retired.Retired = true
	retired.Redirect = b.Endpoint
	if err := m.Merge(MembershipDelta{Members: []Member{retired}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Merge(MembershipDelta{Members: []Member{a}}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Endpoint("a"); err != nil || got != b.Endpoint {
		t.Fatal(got, err)
	}
	moved := b
	moved.Endpoint.Epoch = 3
	moved.Endpoint.Host = "b:4"
	moved.Endpoint.Storage = "disk-b4"
	if err := m.Merge(MembershipDelta{Members: []Member{moved}}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Endpoint("a"); err != nil || got != moved.Endpoint {
		t.Fatal(got, err)
	}
	fork := retired
	fork.Redirect = m.Export().Members[2].Endpoint
	before := m.Digest()
	if err := m.Merge(MembershipDelta{Members: []Member{fork}}); !errors.Is(err, ErrConflict) {
		t.Fatal("retirement successor changed", err)
	}
	if before != m.Digest() {
		t.Fatal("changed on invalid retirement")
	}
}

func TestMembershipDetachedViewsAndXORRouting(t *testing.T) {
	m, a, b := membershipFixture(t)
	if err := m.Merge(MembershipDelta{Members: []Member{a, b}}); err != nil {
		t.Fatal(err)
	}
	original := m.Digest()
	clone := m.Clone()
	if err := clone.Merge(MembershipDelta{Members: []Member{{VNode: a.VNode, Endpoint: a.Endpoint, Retired: true, Redirect: b.Endpoint}}}); err != nil {
		t.Fatal(err)
	}
	if m.Digest() != original {
		t.Fatal("clone mutated original")
	}
	view := m.Current()
	view.Remove("a")
	exported := m.Export()
	exported.Members[0].Endpoint.Host = "mutated"
	if got, err := m.Route(a.VNode.ID); err != nil || got != a.Endpoint {
		t.Fatal(got, err)
	}
	if len(m.Current().Active) != 3 || m.Digest() != original {
		t.Fatal("returned containers alias internal state")
	}
	if _, err := m.Endpoint("missing"); !errors.Is(err, ErrDependency) {
		t.Fatal(err)
	}
	if _, err := m.Resolve(Endpoint{}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestMembershipConcurrentPublication(t *testing.T) {
	m, a, _ := membershipFixture(t)
	if err := m.Merge(MembershipDelta{Members: []Member{a}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if got, err := m.Route(a.VNode.ID); err != nil || got.Owner != a.Endpoint.Owner {
					t.Errorf("route: %+v %v", got, err)
					return
				}
				_ = m.Export()
				_ = m.Clone()
				_ = m.Current()
				_ = m.Digest()
			}
		}()
	}
	for i := uint64(1); i <= 100; i++ {
		update := a
		update.Endpoint.Epoch = i
		if err := m.Merge(MembershipDelta{Members: []Member{update}}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if got, err := m.Endpoint(a.VNode.Name); err != nil || got.Epoch != 100 {
		t.Fatal(got, err)
	}
}

func TestMembershipRejectsInvalidGenesis(t *testing.T) {
	root := tree.VNodeDescriptor{Name: "root", ID: tree.ID{1}}
	genesis := Genesis{Cluster: "test", Holder: Endpoint{Host: "root:1", Owner: Owner{VNode: "root", Storage: "disk"}}}
	invalidRoot := root
	invalidRoot.ID = tree.Zero
	if _, err := NewMembership(invalidRoot, genesis); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted zero root ID", err)
	}
	wrongRoot := root
	wrongRoot.Name = "different"
	if _, err := NewMembership(wrongRoot, genesis); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted conflicting root", err)
	}
}

// This is a deterministic membership/cache schedule, not a 64-disk transfer
// test. It deliberately exercises only routing convergence and state size.
func TestMembership64PeersRepartitionAndDrain(t *testing.T) {
	const nodes, rounds = 64, 12
	descriptor := func(i int) tree.VNodeDescriptor {
		return tree.VNodeDescriptor{Name: fmt.Sprintf("node-%02d", i), ID: tree.ID{byte(i + 1)}}
	}
	endpoint := func(i, epoch int) Endpoint {
		name := descriptor(i).Name
		// Three physical slots are reused, including returns to the original disk.
		return Endpoint{Owner: Owner{VNode: name, Storage: fmt.Sprintf("disk-%02d-%d", i, epoch%3)}, Host: fmt.Sprintf("host-%02d-%d:1", i, epoch%3), Epoch: uint64(epoch)}
	}
	root := descriptor(0)
	rootEndpoint := endpoint(0, 0)
	genesis := Genesis{Cluster: "membership64", Holder: Endpoint{Host: rootEndpoint.Host, Owner: Owner{VNode: root.Name, Storage: rootEndpoint.Storage}}}
	peers := make([]*Membership, nodes)
	permissions := make([]LocalPermissions, nodes)
	for i := range peers {
		var err error
		peers[i], err = NewMembership(root, genesis)
		if err != nil {
			t.Fatal(err)
		}
		if err = peers[i].Merge(MembershipDelta{Members: []Member{{VNode: descriptor(i), Endpoint: endpoint(i, 0)}}}); err != nil {
			t.Fatal(err)
		}
	}
	type delivery struct {
		target int
		delta  MembershipDelta
	}
	rng := rand.New(rand.NewSource(640025))
	delivered := 0
	deliver := func(queue []delivery) {
		rng.Shuffle(len(queue), func(i, j int) { queue[i], queue[j] = queue[j], queue[i] })
		for _, msg := range queue {
			if err := peers[msg.target].Merge(msg.delta); err != nil {
				t.Fatal(err)
			}
			if len(peers[msg.target].Export().Members) > nodes {
				t.Fatal("membership grew with handoffs")
			}
			delivered++
		}
	}
	for epoch := 1; epoch <= rounds; epoch++ {
		// The network changes its four disconnected groups twice. Duplicated
		// announcements are queued with previous endpoints to force old-after-new
		// delivery; no history is needed to ignore these stale versions.
		group := func(i int) int {
			switch {
			case epoch <= 4:
				return i / 16
			case epoch <= 8:
				return i % 4
			default:
				return (i / 8) % 4
			}
		}
		var queue []delivery
		for source := range peers {
			future := endpoint(source, epoch)
			if got, err := peers[source].Resolve(future); err != nil || got != future {
				t.Fatalf("future endpoint before publication: %+v %v", got, err)
			}
			current := Member{VNode: descriptor(source), Endpoint: future}
			old := Member{VNode: descriptor(source), Endpoint: endpoint(source, epoch-1)}
			if err := peers[source].Merge(MembershipDelta{Members: []Member{current}}); err != nil {
				t.Fatal(err)
			}
			for target := range peers {
				if group(source) == group(target) {
					queue = append(queue, delivery{target, MembershipDelta{Members: []Member{current}}}, delivery{target, MembershipDelta{Members: []Member{old, current}}})
				}
			}
		}
		deliver(queue)
	}
	// Partitions heal. Exchange compact snapshots captured before any drain so
	// convergence does not depend on the propagation order or a central oracle.
	var queue []delivery
	for _, source := range peers {
		snapshot := source.Export()
		for target := range peers {
			queue = append(queue, delivery{target, snapshot}, delivery{target, snapshot})
		}
	}
	deliver(queue)
	expected := peers[0].Digest()
	encoded, err := json.Marshal(peers[0].Export())
	if err != nil {
		t.Fatal(err)
	}
	for i, peer := range peers {
		if peer.Digest() != expected {
			t.Fatalf("peer %d did not converge", i)
		}
		if got := len(peer.Export().Members); got != nodes {
			t.Fatalf("peer %d records=%d, want %d", i, got, nodes)
		}
		for target := range peers {
			got, err := peer.Route(descriptor(target).ID)
			if err != nil || got != endpoint(target, rounds) {
				t.Fatalf("peer %d route %d: %+v %v", i, target, got, err)
			}
		}
		// Installing routing announcements cannot create any native permission.
		// Actual storage/fence safety is qualified by the transfer tests, elsewhere.
		if len(permissions[i].Native) != 0 || permissions[i].Sequence != 0 {
			t.Fatal("membership mutated local permissions")
		}
	}
	t.Logf("peers=%d physical_moves=%d delivered_messages=%d records_per_peer=%d compact_json_bytes=%d (descriptive serialization, no latency SLA)", nodes, nodes*rounds, delivered, nodes, len(encoded))
}
