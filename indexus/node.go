package indexus

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/internal/auth"
	"github.com/indexus/go-indexus-core/internal/autoscale"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Node exposes the causal runtime through peer discovery and client APIs.
// Durable data, write authority and handoffs belong exclusively to mesh.Service.
type Node struct {
	mesh              *meshRuntime
	settings          *Settings
	newContact        func(string, map[string]any, int) Contact
	bootstraps        []Contact
	routing           *tree.Trie[Peer]
	registered        *tree.Trie[Contact]
	acknowledged      *tree.Trie[Contact]
	cert              *auth.NodeCert
	autoscale         *autoscale.Controller
	full              atomic.Bool
	items             atomic.Int64
	lastCountAt       atomic.Int64
	measuredOwnership atomic.Pointer[measuredOwnership]
	machine           *Machine
	suspectMu         sync.Mutex
	suspect           map[string]time.Time
	heatMu            sync.Mutex
	zoneHeat          recentHeat
	joinHints         tree.RecentHints
	openTransferSince atomic.Int64
	// transferStallWarned keeps one warning per stall of the open transfers.
	transferStallWarned atomic.Bool
	// joining: the initial join has not completed (see Joining).
	joining atomic.Bool
	// maintenanceHeld: see HoldMaintenance.
	maintenanceHeld atomic.Bool
}

// NewNode creates the network facade. EnableCausalCluster must attach a durable
// service before data operations can run; there is no in-memory fallback.
func NewNode(settings *Settings, newContact func(string, map[string]any, int) Contact, bootstraps []Contact) (*Node, error) {
	if settings == nil || newContact == nil || settings.name == "" || FromSlice(settings.id).IsZero() {
		return nil, fmt.Errorf("invalid node configuration")
	}
	node := &Node{settings: settings, newContact: newContact, bootstraps: bootstraps,
		routing: tree.NewTrie[Peer](), registered: tree.NewTrie[Contact](), acknowledged: tree.NewTrie[Contact](),
		machine: NewMachine(settings.name, 1<<62), suspect: map[string]time.Time{}}
	node.register([]Contact{node})
	node.acknowledge(bootstraps)
	return node, nil
}

func (n *Node) ID() []byte {
	return n.settings.id
}

func (n *Node) Name() string {
	return n.settings.name
}

func (n *Node) IPs() map[string]any {
	return n.settings.ips
}

func (n *Node) Port() int {
	return n.settings.port
}

func (n *Node) IP() string {
	return n.settings.ip
}

func (n *Node) Host() string {
	return fmt.Sprintf("%s@%s|%d", n.settings.name, n.settings.ip, n.settings.port)
}

func (n *Node) Cert() *auth.NodeCert {
	return n.cert
}

func (n *Node) SetCert(cert *auth.NodeCert) {
	n.cert = cert
}

func (n *Node) Delay() time.Duration {
	if n.autoscale != nil && n.autoscale.Config().Role == "spawned" && !n.ClientReady() {
		return 2 * time.Second
	}
	return n.settings.delay
}
