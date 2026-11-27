package mesh

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Reopen copies of captured disks without liveness/summary/autoscale jobs.
// This is an offline diagnostic, not a repeat of the physical qualification.
// Original stores and private identity files are never edited or published.
func TestReplayCapturedInventory(t *testing.T) {
	root := os.Getenv("INDEXUS_INVENTORY_REPLAY")
	if root == "" {
		t.Skip("opt-in: INDEXUS_INVENTORY_REPLAY=/absolute/captured-run")
	}
	f, err := os.Open(filepath.Join(root, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type observed struct {
		Host    string
		Slot    int
		Metrics struct{ Name string }
	}
	var last struct{ Nodes []observed }
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		if err := json.Unmarshal(scanner.Bytes(), &last); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(last.Nodes, func(i, j int) bool {
		return fmt.Sprint(last.Nodes[i].Host, last.Nodes[i].Slot) < fmt.Sprint(last.Nodes[j].Host, last.Nodes[j].Slot)
	})
	net := &network{nodes: map[string]*Service{}, blocked: map[string]bool{}}
	wire := &coverageJSONTransport{calls: map[string]int{}, handle: func(host string, r Request) (Response, error) {
		if !inventoryRPC(r.Kind) || r.Membership != nil {
			return Response{}, fmt.Errorf("offline replay attempted control RPC %s", r.Kind)
		}
		return net.Exchange(context.Background(), host, r)
	}}
	var nodes []*Service
	for _, n := range last.Nodes {
		if n.Metrics.Name == "" {
			continue
		}
		source := filepath.Join(root, n.Host, "slots", fmt.Sprint(n.Slot))
		copy := filepath.Join(t.TempDir(), "slot")
		if err := os.CopyFS(copy, os.DirFS(source)); err != nil {
			t.Fatal(err)
		}
		store, err := storage.OpenWithOptions(filepath.Join(copy, "backup.causal.db"), n.Metrics.Name, "experiment", 1<<30, storage.Options{NoSync: true, MemoryBytes: 16 << 20})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		routing, err := store.CausalRouting()
		if err != nil {
			t.Fatal(err)
		}
		self, err := routing.Placement(n.Metrics.Name)
		if err != nil {
			t.Fatal(err)
		}
		id, ok := routing.Current().IDOf(n.Metrics.Name)
		if !ok {
			t.Fatal("captured node is not active")
		}
		service := &Service{Store: store, Self: self, VNode: tree.VNodeDescriptor{Name: n.Metrics.Name, ID: id}, Peers: wire}
		t.Cleanup(service.Close)
		nodes = append(nodes, service)
		net.nodes[self.Host] = service
	}
	if len(nodes) == 0 {
		t.Fatal("no captured nodes")
	}
	for _, node := range nodes {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rows, err := node.QueryPrefixMetrics(ctx, "Placeh0000000000", "@")
		cancel()
		count := 0
		for _, snapshot := range rows {
			count += len(snapshot.Records)
		}
		t.Logf("entry=%s zones=%d records=%d err=%v", node.Self.VNode, len(rows), count, err)
		if err != nil || count != 8998 {
			t.Errorf("captured hot collection unavailable or incomplete: %d %v", count, err)
		}
	}
}
