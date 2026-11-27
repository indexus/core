package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/http/p2p"
	"github.com/indexus/go-indexus-core/internal/adapter/peer"
	"github.com/indexus/go-indexus-core/internal/storage"
)

type runningDataNode struct {
	node     *indexus.Node
	boot     *clusterBoot
	server   *p2p.Handler
	port     int
	stopOnce sync.Once
}

func startDataNode(t *testing.T, name string, bootstrap *runningDataNode) *runningDataNode {
	t.Helper()
	config := Config{Name: name, Storage: filepath.Join(t.TempDir(), "node"), Advertise: []string{"127.0.0.1"}, ClusterID: "integration", ClusterCapacity: 1 << 24, CreateCluster: true}
	if bootstrap != nil {
		config.Bootstraps = []indexus.Contact{peer.NewContact(bootstrap.node.Name(), map[string]any{"127.0.0.1": nil}, bootstrap.port)}
	}
	return startConfiguredDataNode(t, config)
}

func startConfiguredDataNode(t *testing.T, config Config) *runningDataNode {
	t.Helper()
	name := config.Name
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", config.P2pPort))
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	config.P2pPort = port
	settings, err := indexus.NewSettings(name, port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	n, err := indexus.NewNode(settings, peer.NewContact, config.Bootstraps)
	if err != nil {
		t.Fatal(err)
	}
	server := p2p.NewHttpHandler("", n, peer.NewContact)
	boot, err := configureCluster(config, n, server, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	running := &runningDataNode{node: n, boot: boot, server: server, port: port}
	t.Cleanup(running.stop)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := boot.Start(ctx, n); err != nil {
		t.Fatalf("start %s:%s: %v", name, strconv.Itoa(port), err)
	}
	return running
}
func TestDurableRuntimeHTTPJoinPreservesWritesAndDelete(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "record", Metrics: []float64{7}, RequestID: "put-1"}
	if err := source.node.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	targetName := indexus.BASE64.Encode(zone.Bytes())
	target := startDataNode(t, targetName, source)
	for _, n := range []*runningDataNode{source, target} {
		_, set, err := n.node.Get("Demo000000000000", "@", true, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if set == nil || set.Count() != 1 {
			t.Fatalf("after join count: %v", set)
		}
	}
	deletion := *item
	deletion.RequestID = "delete-2"
	if err := source.node.Delete(&deletion, "@", nil); err != nil {
		t.Fatal(err)
	}
	// Retry the original accepted put through the old owner. Its operation ID
	// travelled with the snapshot and must not resurrect the newer tombstone.
	if err := source.node.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	_, set, err := source.node.Get("Demo000000000000", "@", true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if set == nil || set.Count() != 0 {
		t.Fatalf("old retry resurrected delete: %v", set)
	}
}

func TestDurableRuntimeSimultaneousJoins(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	if err := source.node.New(&indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "before-joins", RequestID: "put-before"}, "@", nil); err != nil {
		t.Fatal(err)
	}
	joined := make([]*runningDataNode, 3)
	var workers sync.WaitGroup
	for i, name := range []string{"AAAAAAAAAAAAAAAC", "AAAAAAAAAAAAAAAD", "AAAAAAAAAAAAAAAE"} {
		workers.Add(1)
		go func(i int, name string) { defer workers.Done(); joined[i] = startDataNode(t, name, source) }(i, name)
	}
	workers.Wait()
	if t.Failed() {
		return
	}
	var hosts []string
	for _, n := range joined {
		hosts = append(hosts, n.boot.endpoint)
	}
	if err := source.boot.mesh.Sync(context.Background(), hosts); err != nil {
		t.Fatal(err)
	}
	state, err := source.boot.store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	replica, err := state.MembershipReplica()
	if err != nil {
		t.Fatal(err)
	}
	if replica.Current().Len() != 4 {
		t.Fatalf("lost concurrent activation: %+v", replica.Current())
	}

	for _, n := range append(joined, source) {
		_, set, err := n.node.Get("Demo000000000000", "@", true, nil, false)
		if err != nil || set == nil || set.Count() != 1 {
			t.Fatalf("data after concurrent joins: set=%v err=%v", set, err)
		}
	}
}

func (n *runningDataNode) stop() {
	n.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = n.server.Shutdown(ctx)
		_ = n.boot.Close()
	})
}

func TestDurableRuntimeRestartAndMissingDisk(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "survives-restart", RequestID: "restart-put"}
	if err := source.node.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	config := source.boot.config
	source.stop()
	restarted := startConfiguredDataNode(t, config)
	_, set, err := restarted.node.Get("Demo000000000000", "@", true, nil, false)
	if err != nil || set == nil || set.Count() != 1 {
		t.Fatalf("restart lost accepted data: %v %v", set, err)
	}
	if err := restarted.node.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	state, err := restarted.boot.store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := storage.Open(filepath.Join(t.TempDir(), "replacement.db"), config.Name, config.ClusterID, 1<<24)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := replacement.InitCausal(state.Root, state.Genesis); !errors.Is(err, storage.ErrMissingData) {
		t.Fatalf("empty volume activated: %v", err)
	}

}

func TestDurableRootRecord(t *testing.T) {
	n := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	item := &indexus.Item{Collection: "Demo000000000000", Location: "@", Id: "root-row", RequestID: "root-put"}
	if err := n.node.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	_, set, err := n.node.Get("Demo000000000000", "@", true, nil, false)
	if err != nil || set == nil || set.Count() != 1 {
		t.Fatalf("accepted root record disappeared: %v %v", set, err)
	}
	if count, err := n.node.Count(); err != nil || count != 1 {
		t.Fatalf("root projection: %d %v", count, err)
	}
	item.RequestID = "root-delete"
	if err := n.node.Delete(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	_, set, err = n.node.Get("Demo000000000000", "@", true, nil, false)
	if err != nil || set == nil || set.Count() != 0 {
		t.Fatalf("root delete: %v %v", set, err)
	}
}

func TestDurableJoinThroughDataOnlyBootstrap(t *testing.T) {
	voter := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	relay := startDataNode(t, "AAAAAAAAAAAAAAAC", voter)

	joiner := startDataNode(t, "AAAAAAAAAAAAAAAD", relay)
	if err := joiner.node.New(&indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "relay-join", RequestID: "relay-put"}, "@", nil); err != nil {
		t.Fatal(err)
	}
	_, set, err := voter.node.Get("Demo000000000000", "@", true, nil, false)
	if err != nil || set == nil || set.Count() != 1 {
		t.Fatalf("data-only bootstrap failed routing: %v %v", set, err)
	}
}

func TestDurableHostMovePreservesRowsAndRetryHistory(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	target := startDataNode(t, indexus.BASE64.Encode(zone.Bytes()), source)
	row := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "row", Metrics: []float64{1}, RequestID: "leave-put"}
	if err := source.node.New(row, "@", nil); err != nil {
		t.Fatal(err)
	}
	if count, err := target.node.Count(); err != nil || count != 1 {
		t.Fatalf("target did not own row: %d %v", count, err)
	}
	config := target.boot.config
	config.Storage = filepath.Join(t.TempDir(), "moved")
	config.P2pPort = 0
	config.MoveFrom = target.boot.endpoint
	moved := startConfiguredDataNode(t, config)
	if err := moved.boot.mesh.Sync(context.Background(), []string{source.boot.endpoint}); err != nil {
		t.Fatal(err)
	}
	target.stop()
	deletion := *row
	deletion.RequestID = "after-move-delete"
	if err := source.node.Delete(&deletion, "@", nil); err != nil {
		t.Fatal(err)
	}
	if err := source.node.New(row, "@", nil); err != nil {
		t.Fatal(err)
	}
	_, set, err := source.node.Get("Demo000000000000", "@", true, nil, false)
	if err != nil || set == nil || set.Count() != 0 {
		t.Fatalf("move lost replay identity: %v %v", set, err)
	}
}
