package node

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/mesh"
)

// A write refreshes its own zone at once; same-vnode ancestors are folded by
// the next Update, never on the write path.
func TestCausalSummarySameVNodeFoldsOnUpdate(t *testing.T) {
	node := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "local", Metrics: []float64{7}, RequestID: "local-1"}
	if err := node.node.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	aggregates := ownerAggregates(t, []*runningDataNode{node}, "Demo000000000000", []string{"aa"})
	if aggregates["aa"].Count() != 1 || aggregates["aa"].Metric(0) != 7 {
		t.Fatalf("leaf: %v", aggregates["aa"])
	}
	if err := node.node.Update(); err != nil {
		t.Fatal(err)
	}
	aggregates = ownerAggregates(t, []*runningDataNode{node}, "Demo000000000000", []string{"@", "a"})
	for _, location := range []string{"a", "@"} {
		if aggregates[location].Count() != 1 || aggregates[location].Metric(0) != 7 {
			t.Fatalf("%s after Update: %v", location, aggregates[location])
		}
	}
}

func TestCausalSummaryPushCoalescesRemoteParent(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	target := startDataNode(t, indexus.BASE64.Encode(zone.Bytes()), source)
	meter := &summaryKindMeter{Transport: target.boot.mesh.Peers}
	target.boot.mesh.Peers = meter
	for i := range 8 {
		item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "row", Metrics: []float64{float64(i + 1)}, RequestID: fmt.Sprintf("ramp-%d", i)}
		if err := source.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := meter.kind(mesh.KindSummaryContribute); got > 2 {
		t.Fatalf("burst issued %d contributes, want coalesced", got)
	}
	if err := target.node.Update(); err != nil {
		t.Fatal(err)
	}
	if err := source.node.Update(); err != nil {
		t.Fatal(err)
	}
	root := ownerAggregates(t, []*runningDataNode{source, target}, "Demo000000000000", []string{"@"})["@"]
	if root.Count() != 1 || root.Metric(0) != 8 {
		t.Fatalf("final parent %v want 8", root)
	}
}

func TestCausalSummaryPushBatchesDistinctRemoteEdges(t *testing.T) {
	source := startDataNode(t, "0000000000000001", nil)
	const baseCollection = "0AABCDEFGH123456"
	zone, err := indexus.ZoneID(baseCollection, "0W")
	if err != nil {
		t.Fatal(err)
	}
	target := startDataNode(t, indexus.BASE64.Encode(zone.Bytes()), source)
	meter := &summaryKindMeter{Transport: target.boot.mesh.Peers}
	target.boot.mesh.Peers = meter
	const edges = 64
	replica, err := target.boot.mesh.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	type boundary struct{ collection, location string }
	var boundaries []boundary
	for i := 0; i < 1<<16 && len(boundaries) < edges; i++ {
		// 16 BASE64 characters = 96 bits. Keeping "0A" fixes the bits used
		// by parent "0"; the 14-character suffix varies distinct leaf zones.
		collection := "0A" + fmt.Sprintf("%014x", i)
		location := "0W"
		leaf, e := indexus.NewZoneKey(collection, location)
		if e != nil {
			continue
		}
		parent, e := indexus.NewZoneKey(collection, indexus.BASE64.Parent(location))
		if e != nil {
			continue
		}
		leafOwner, _ := replica.Current().OwnerName(leaf.ID)
		parentOwner, _ := replica.Current().OwnerName(parent.ID)
		if leafOwner == target.boot.mesh.VNode.Name && parentOwner == source.boot.mesh.VNode.Name {
			boundaries = append(boundaries, boundary{collection, location})
		}
	}
	if len(boundaries) != edges {
		leaf, _ := indexus.NewZoneKey(baseCollection, "0W")
		parent, _ := indexus.NewZoneKey(baseCollection, "0")
		leafOwner, _ := replica.Current().OwnerName(leaf.ID)
		parentOwner, _ := replica.Current().OwnerName(parent.ID)
		t.Fatalf("found %d remote boundary edges, want %d; active=%d leaf=%s parent=%s source=%s target=%s", len(boundaries), edges, replica.Current().Len(), leafOwner, parentOwner, source.boot.mesh.VNode.Name, target.boot.mesh.VNode.Name)
	}
	writeStarted := time.Now()
	for i, boundary := range boundaries {
		item := &indexus.Item{
			Collection: boundary.collection,
			Location:   boundary.location,
			Id:         "row",
			Metrics:    []float64{1},
			RequestID:  fmt.Sprintf("batch-%03d", i),
		}
		if err := source.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := target.node.Update(); err != nil {
		t.Fatal(err)
	}
	// Durable writes may span several coalescing windows on a busy machine.
	// Bound RPCs by elapsed windows instead of assuming all 64 commits take <1s.
	maxCalls := int(time.Since(writeStarted)/time.Second) + 2
	if calls, values, largest := meter.summaryTraffic(); calls > maxCalls || values < edges || largest < 2 {
		t.Fatalf("distinct edges were not batched: calls=%d values=%d largest=%d", calls, values, largest)
	}
}

func TestCausalSummaryDuplicateContributeDoesNotDoubleCount(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	target := startDataNode(t, indexus.BASE64.Encode(zone.Bytes()), source)
	item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "dup", Metrics: []float64{4}, RequestID: "dup-1"}
	if err := source.node.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := target.node.Update(); err != nil {
			t.Fatal(err)
		}
		if err := source.node.Update(); err != nil {
			t.Fatal(err)
		}
	}
	first := ownerAggregates(t, []*runningDataNode{source, target}, "Demo000000000000", []string{"@"})["@"]
	for range 3 {
		if err := target.node.Update(); err != nil {
			t.Fatal(err)
		}
		if err := source.node.Update(); err != nil {
			t.Fatal(err)
		}
	}
	again := ownerAggregates(t, []*runningDataNode{source, target}, "Demo000000000000", []string{"@"})["@"]
	if !first.IsEqual(again) {
		t.Fatalf("repeated contribute changed total %v -> %v", first, again)
	}
}

func TestCausalSummaryConcurrentWrites(t *testing.T) {
	node := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	var wait sync.WaitGroup
	var failed atomic.Bool
	for i := range 32 {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: fmt.Sprintf("r%d", i), Metrics: []float64{1}, RequestID: fmt.Sprintf("c-%d", i)}
			if err := node.node.New(item, "@", nil); err != nil {
				failed.Store(true)
			}
		}(i)
	}
	wait.Wait()
	if failed.Load() {
		t.Fatal("concurrent write failed")
	}
	if err := node.node.Update(); err != nil {
		t.Fatal(err)
	}
	root := ownerAggregates(t, []*runningDataNode{node}, "Demo000000000000", []string{"@"})["@"]
	if root.Count() != 32 {
		t.Fatalf("count=%d want 32", root.Count())
	}
}

type summaryKindMeter struct {
	mesh.Transport
	mu                  sync.Mutex
	kinds               map[string]int
	summaryValues       int
	largestSummaryBatch int
}

func (m *summaryKindMeter) Exchange(ctx context.Context, host string, request mesh.Request) (mesh.Response, error) {
	m.mu.Lock()
	if m.kinds == nil {
		m.kinds = map[string]int{}
	}
	m.kinds[request.Kind]++
	if request.Kind == mesh.KindSummaryContribute {
		values := len(request.Contributions)
		m.summaryValues += values
		m.largestSummaryBatch = max(m.largestSummaryBatch, values)
	}
	m.mu.Unlock()
	return m.Transport.Exchange(ctx, host, request)
}

func (m *summaryKindMeter) kind(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kinds[kind]
}

func (m *summaryKindMeter) summaryTraffic() (calls, values, largest int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kinds[mesh.KindSummaryContribute], m.summaryValues, m.largestSummaryBatch
}

func TestCausalSummaryParentRestartPollReconstructs(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	target := startDataNode(t, indexus.BASE64.Encode(zone.Bytes()), source)
	item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "keep", Metrics: []float64{6}, RequestID: "keep-1"}
	if err := source.node.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_ = target.node.Update()
		_ = source.node.Update()
	}
	config := source.boot.config
	source.stop()
	source = startConfiguredDataNode(t, config)
	deadline := time.Now().Add(8 * time.Second)
	for {
		_ = target.node.Update()
		_ = source.node.Update()
		for _, node := range []*runningDataNode{source, target} {
			aggregates, err := node.node.GetAggregates("Demo000000000000", []string{"@"})
			if err == nil {
				if root, ok := aggregates["@"]; ok && root.Count() == 1 && root.Metric(0) == 6 {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent restart did not reconstruct @")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A joiner publishes its activation after installing its first region, so
// the install-time fold runs while the region is not writable yet and sees
// nothing. The activation must fold the installed rows itself: the joiner
// answers for its new zone right after the join, before any Update tick.
func TestCausalJoinerAggregatesFreshWithoutUpdate(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	const rows = 6
	for i := range rows {
		item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: fmt.Sprintf("r%d", i), Metrics: []float64{1}, RequestID: fmt.Sprintf("fresh-%d", i)}
		if err := source.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	target := startDataNode(t, indexus.BASE64.Encode(zone.Bytes()), source)
	aggregates, err := target.node.GetAggregates("Demo000000000000", []string{"aa"})
	if err != nil {
		t.Fatal(err)
	}
	if got := aggregates["aa"]; got == nil || got.Count() != rows {
		t.Fatalf("joiner answers %v for its installed zone before any Update, want count=%d", got, rows)
	}
}
