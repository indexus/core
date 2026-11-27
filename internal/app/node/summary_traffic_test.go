package node

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/testkit"
)

// Measure the current production refresh, including HTTP serialization sizes.
// Counters exclude setup, topology convergence and user writes.
type summaryTraffic struct {
	mesh.Transport
	mu                    sync.Mutex
	calls, records, bytes int
}

func (m *summaryTraffic) Exchange(ctx context.Context, host string, request mesh.Request) (mesh.Response, error) {
	response, err := m.Transport.Exchange(ctx, host, request)
	if request.Kind == "inventory" {
		requestJSON, _ := json.Marshal(request)
		responseJSON, _ := json.Marshal(response)
		m.mu.Lock()
		m.calls++
		m.bytes += len(requestJSON) + len(responseJSON)
		for _, zone := range response.Inventory {
			m.records += len(zone.Records)
		}
		m.mu.Unlock()
	}
	return response, err
}

func (m *summaryTraffic) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls, m.records, m.bytes = 0, 0, 0
}
func (m *summaryTraffic) totals() (int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, m.records, m.bytes
}

// This diagnostic intentionally measures the all-to-all implementation rather
// than asserting it is scalable. Keep it out of the short test suite.
func TestPeriodicSummaryTraffic(t *testing.T) {
	testkit.RequireLong(t)
	for _, count := range []int{1, 4, 8} {
		t.Run(fmt.Sprintf("nodes-%d", count), func(t *testing.T) {
			ctx := context.Background()
			var nodes []*runningDataNode
			var hosts []string
			for i := range count {
				name := indexus.BASE64.Encode(indexus.ID{byte(i * 256 / count), 1}.Bytes())
				var bootstrap *runningDataNode
				if i > 0 {
					bootstrap = nodes[0]
				}
				node := startDataNode(t, name, bootstrap)
				nodes = append(nodes, node)
				hosts = append(hosts, node.boot.endpoint)
			}
			for range 3 {
				for _, node := range nodes {
					if err := node.boot.mesh.Sync(ctx, hosts); err != nil {
						t.Fatal(err)
					}
					if err := node.boot.mesh.Resume(ctx); err != nil {
						t.Fatal(err)
					}
					if err := node.boot.mesh.Rebalance(ctx); err != nil {
						t.Fatal(err)
					}
				}
			}
			meters := make([]*summaryTraffic, count)
			for i, node := range nodes {
				meters[i] = &summaryTraffic{Transport: node.boot.mesh.Peers}
				node.boot.mesh.Peers = meters[i]
			}
			for _, depth := range []int{1, 12} {
				collection := indexus.BASE64.Encode(indexus.ID{byte(depth), 2}.Bytes())
				for i := range 32 {
					item := &indexus.Item{Collection: collection, Location: strings.Repeat("a", depth), Id: fmt.Sprintf("r%d", i), Metrics: []float64{float64(i + 1)}, RequestID: fmt.Sprintf("%s-r%d", collection, i)}
					if err := nodes[0].node.New(item, "@", nil); err != nil {
						t.Fatal(err)
					}
				}
				for _, meter := range meters {
					meter.reset()
				}
				for _, node := range nodes {
					if err := node.node.Update(); err != nil {
						t.Fatal(err)
					}
				}
				owner := ownerAggregates(t, nodes, collection, []string{"@"})["@"]
				if owner.Count() != 32 || owner.Metric(0) != 528 {
					t.Fatalf("wrong root: %v", owner)
				}
				calls, records, payload := 0, 0, 0
				for _, meter := range meters {
					c, r, b := meter.totals()
					calls += c
					records += r
					payload += b
				}
				if calls != 0 {
					t.Fatalf("refresh still used inventory fanout: %d", calls)
				}
				// Repeated reads of a published parent must not trigger fanout.
				for range 100 {
					if _, err := nodes[0].node.GetAggregates(collection, []string{"@", "a"}); err != nil {
						t.Fatal(err)
					}
				}
				readCalls := 0
				for _, meter := range meters {
					c, _, _ := meter.totals()
					readCalls += c
				}
				if readCalls != calls {
					t.Fatal("parent reads triggered remote refreshes")
				}
				t.Logf("nodes=%d depth=%d refresh_rpcs=%d transferred_records=%d encoded_payload_bytes=%d extra_rpcs_for_100_reads=%d", count, depth, calls, records, payload, readCalls-calls)
				for _, meter := range meters {
					meter.reset()
				}
				for _, node := range nodes {
					if err := node.node.Update(); err != nil {
						t.Fatal(err)
					}
				}
				idleCalls := 0
				for _, meter := range meters {
					c, _, _ := meter.totals()
					idleCalls += c
				}
				if idleCalls != 0 {
					t.Fatalf("idle refresh used inventory fanout: %d", idleCalls)
				}
				t.Logf("nodes=%d depth=%d unchanged_inventory_rpcs=%d", count, depth, idleCalls)
			}
		})
	}
}
