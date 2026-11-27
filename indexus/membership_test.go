package indexus

import (
	"errors"
	"fmt"
	"testing"
)

type recoveringPeer struct {
	*dialPeer
	online bool
}

func (p *recoveringPeer) Ping(Contact) (Contact, error) {
	if !p.online {
		return nil, errors.New("partitioned")
	}
	return p, nil
}

func TestObserveReprobesAndRestoresQuarantinedPeer(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)
	peer := &recoveringPeer{
		dialPeer: newDialPeer("RecoveringPeerAA", "10.0.0.2", 21002),
	}
	n.register([]Contact{peer})

	if err := n.Observe(); err != nil {
		t.Fatalf("Observe partition: %v", err)
	}
	if !n.suspended(peer.Name()) {
		t.Fatal("failed peer was not quarantined")
	}
	if _, ok := n.acknowledged.Get(peer.ID()); !ok {
		t.Fatal("quarantined peer lost its probationary probe address")
	}

	peer.online = true
	if err := n.Observe(); err != nil {
		t.Fatalf("Observe heal: %v", err)
	}
	if n.suspended(peer.Name()) {
		t.Fatal("successful direct ping did not clear quarantine")
	}
	if _, ok := n.registered.Get(peer.ID()); !ok {
		t.Fatal("recovered peer was not registered again")
	}
}

func TestContactDialable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		peer *dialPeer
		want bool
	}{
		{"nil", nil, false},
		{"port zero", newDialPeer("PeerAAAAAAAAAAAA", "10.0.0.1", 0), false},
		{"port negative", newDialPeer("PeerAAAAAAAAAAAA", "10.0.0.1", -1), false},
		{"empty endpoint", &dialPeer{name: "PeerAAAAAAAAAAAA", id: []byte{1}, port: 21000}, false},
		{"primary IP", newDialPeer("PeerAAAAAAAAAAAA", "10.0.0.1", 21000), true},
		{"ips map only", &dialPeer{
			name: "PeerAAAAAAAAAAAA",
			id:   []byte{1},
			port: 21000,
			ips:  map[string]any{"10.0.0.2": nil},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Contact
			if tc.peer != nil {
				c = tc.peer
			}
			if got := contactDialable(c); got != tc.want {
				t.Fatalf("contactDialable=%v want %v", got, tc.want)
			}
		})
	}
}

func TestRegisterRejectsUndialable(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)
	dead := newDialPeer("DeadPeerAAAAAAAA", "10.0.0.9", 0)
	n.register([]Contact{dead})

	reg, err := n.Registered()
	if err != nil {
		t.Fatalf("Registered: %v", err)
	}
	for _, c := range reg {
		if c.Name() == dead.Name() {
			t.Fatalf("registered undialable peer %s", dead.Name())
		}
	}
}

func TestRegisterRefreshesStaleContact(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)

	stale := &dialPeer{
		name: "PeerAAAAAAAAAAAA",
		id:   mustDecode(t, "PeerAAAAAAAAAAAA"),
		port: 21000,
		ips:  map[string]any{"10.0.0.1": nil},
	}
	fresh := newDialPeer("PeerAAAAAAAAAAAA", "10.0.0.2", 21000)

	n.register([]Contact{stale})
	n.register([]Contact{fresh})

	reg, err := n.Registered()
	if err != nil {
		t.Fatalf("Registered: %v", err)
	}
	found := false
	for _, c := range reg {
		if c.Name() != fresh.Name() {
			continue
		}
		found = true
		if c.IP() != "10.0.0.2" {
			t.Fatalf("stale contact kept: IP=%q want 10.0.0.2", c.IP())
		}
	}
	if !found {
		t.Fatal("refreshed contact missing from Registered")
	}
}

func TestGetDoesNotRedirectToUndialable(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	root := BASE64.Root()

	queryID, err := MergeEncodings(BASE64, BASE64, root, "Demo000000000000")
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	dead := &dialPeer{
		name: "DeadPeerAAAAAAAA",
		id:   queryID,
		port: 0,
		ip:   "10.0.0.9",
		ips:  map[string]any{"10.0.0.9": nil},
	}

	n.registered.Insert(dead.ID(), dead)

	contact, set, err := n.Get("Demo000000000000", root, true, nil, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set == nil || set.Count() == 0 {
		t.Fatal("expected local set")
	}
	if contact.Name() != n.Name() {
		t.Fatalf("redirected to %s; want self", contact.Name())
	}
}

func mustDecode(t *testing.T, name string) []byte {
	t.Helper()
	id, err := BASE64.Decode(name)
	if err != nil {
		t.Fatalf("Decode(%q): %v", name, err)
	}
	return id
}

func TestObserveSubscribesNewPeersIntoRouting(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)
	peer := newDialPeer("PeerBBBBBBBBBBBB", "10.0.0.2", 21001)
	n.acknowledge([]Contact{peer})

	if err := n.Observe(); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	reg, err := n.Registered()
	if err != nil {
		t.Fatalf("Registered: %v", err)
	}
	foundReg := false
	for _, c := range reg {
		if c.Name() == peer.Name() {
			foundReg = true
			break
		}
	}
	if !foundReg {
		t.Fatal("Observe did not register the acknowledged peer")
	}

	rte, err := n.Routing()
	if err != nil {
		t.Fatalf("Routing: %v", err)
	}
	foundRte := false
	for _, c := range rte {
		if c.Name() == peer.Name() {
			foundRte = true
			break
		}
	}
	if !foundRte {
		t.Fatal("Observe registered the peer but left routing empty — mesh asymmetry")
	}
}

func TestRegisterDoesNotResurrectAQuarantinedPeer(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)
	peer := newDialPeer("QuarantinedAAAAA", "10.0.0.2", 21001)

	n.suspend(peer.Name(), quarantineWindow)
	n.reject([]Contact{peer})
	if n.register([]Contact{peer}) {
		t.Fatal("register reported a new insert for a quarantined peer")
	}

	if _, exist := n.registered.Get(peer.ID()); exist {
		t.Fatal("gossip put a peer that stopped answering back in the routing table")
	}
}

func TestRegisterReportsOnlyNewInserts(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)
	peer := newDialPeer("PeerBBBBBBBBBBBB", "10.0.0.2", 21001)

	if !n.register([]Contact{peer}) {
		t.Fatal("first register should report a new insert")
	}
	if n.register([]Contact{peer}) {
		t.Fatal("re-registering a known peer must not report new")
	}

	n.suspend(peer.Name(), quarantineWindow)
	n.reject([]Contact{peer})
	if n.register([]Contact{peer}) {
		t.Fatal("quarantined peer must not count as a new insert")
	}
}

func filledNode(t *testing.T, locations []string, per int) *Node {
	t.Helper()

	node := newNodeOn(t, 8)
	for _, location := range locations {
		for i := 0; i < per; i++ {
			item := &Item{
				Collection: "Demo000000000000",
				Location:   location,
				Id:         fmt.Sprintf("%s-%d", location, i),
				Metrics:    []float64{1},
			}
			if err := node.New(item, BASE64.Root(), nil); err != nil {
				t.Fatalf("New: %v", err)
			}
		}
	}
	return node
}

func TestItemsDoesNotRecountOnEveryCall(t *testing.T) {
	t.Parallel()
	node := filledNode(t, []string{"aa", "ab", "ba", "zz"}, 12)

	node.MeasureItems()
	if got, want := node.Items(), 48; got != want {
		t.Fatalf("Items: got %d, want %d", got, want)
	}

	if err := node.Delete(&Item{Collection: "Demo000000000000", Location: "aa", Id: "aa-0"}, "@", nil); err != nil {
		t.Fatal(err)
	}
	exact, err := node.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if exact != 47 {
		t.Fatalf("Count: got %d, want 47", exact)
	}

	node.MeasureItems()
	if got := node.Items(); got != exact {
		t.Fatalf("Items after a measure: got %d, want %d", got, exact)
	}
}

func TestObserveMeasuresItems(t *testing.T) {
	t.Parallel()
	node := filledNode(t, []string{"aa", "ab"}, 5)

	if got := node.items.Load(); got != 0 {
		t.Fatalf("precondition: items already measured (%d)", got)
	}
	if err := node.Observe(); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got, want := node.items.Load(), int64(10); got != want {
		t.Fatalf("items after Observe: got %d, want %d", got, want)
	}
}
