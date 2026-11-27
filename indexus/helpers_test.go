package indexus

import (
	"fmt"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"path/filepath"
	"testing"
	"time"
)

type dialPeer struct {
	name string
	id   []byte
	ip   string
	ips  map[string]any
	port int
}

func testNewContact(name string, ips map[string]any, port int) Contact {
	ip := "127.0.0.1"
	for k := range ips {
		ip = k
		break
	}
	return newDialPeer(name, ip, port)
}

func newDialPeer(name, ip string, port int) *dialPeer {
	id, err := BASE64.Decode(name)
	if err != nil {
		id = make([]byte, 12)
		copy(id, []byte(name))
	}
	ips := map[string]any{}
	if ip != "" {
		ips[ip] = nil
	}
	return &dialPeer{name: name, id: id, ip: ip, ips: ips, port: port}
}

func (p *dialPeer) Name() string                  { return p.name }
func (p *dialPeer) ID() []byte                    { return p.id }
func (p *dialPeer) Hash() string                  { return p.name }
func (p *dialPeer) IPs() map[string]any           { return p.ips }
func (p *dialPeer) Port() int                     { return p.port }
func (p *dialPeer) IP() string                    { return p.ip }
func (p *dialPeer) Host() string                  { return fmt.Sprintf("%s@%s|%d", p.name, p.ip, p.port) }
func (p *dialPeer) Ping(Contact) (Contact, error) { return p, nil }
func (p *dialPeer) Neighbors(Peer) ([]Contact, error) {
	return nil, nil
}
func (p *dialPeer) Random(Peer) (Contact, error) { return nil, nil }
func (p *dialPeer) Get(string, string, bool, Visited, bool) (Contact, *Set, error) {
	return p, nil, nil
}
func (p *dialPeer) New(*Item, string, Visited) error    { return nil }
func (p *dialPeer) Delete(*Item, string, Visited) error { return nil }

func item(id string) *Item {
	return &Item{Collection: "Demo000000000000", Location: "aa", Id: id, Metrics: []float64{1}}
}
func newNodeOn(t testing.TB, delegation int) *Node {
	t.Helper()
	name, err := BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := NewSettings(name, 21000, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	settings.SetAdvertise("127.0.0.1")
	node, err := NewNode(settings, testNewContact, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), name, "unit-test", 1<<27, testStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	holder, err := store.Endpoint("http://127.0.0.1:21000")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.InitCausal(VNodeDescriptor{Name: name, ID: FromSlice(node.ID())}, causal.Genesis{Cluster: "unit-test", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	transport := &readPhaseTransport{nodes: map[string]*Node{holder.Host: node}}
	if err = node.EnableCausalCluster(&mesh.Service{Store: store, Self: holder, VNode: VNodeDescriptor{Name: name, ID: FromSlice(node.ID())}, Peers: transport}); err != nil {
		t.Fatal(err)
	}
	return node
}
func ownedNode(t *testing.T) *Node {
	t.Helper()
	node := newNodeOn(t, 64)
	if err := node.New(item("x"), BASE64.Root(), nil); err != nil {
		t.Fatal(err)
	}
	return node
}
func (n *Node) noteZoneWrite(collection, location string) {
	n.heatZone(Key{Collection: collection, Location: location})
}
