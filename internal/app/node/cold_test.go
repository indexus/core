package node

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/peer"
)

func TestCausalHTTPCompactionAndRetiredRestart(t *testing.T) {
	config := Config{CreateCluster: true, Name: "AAAAAAAAAAAAAAAB", Storage: filepath.Join(t.TempDir(), "source"), Advertise: []string{"127.0.0.1"}, ClusterID: "cold-http", ClusterCapacity: 1 << 26}
	source := startConfiguredDataNode(t, config)
	row := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "before", Metrics: []float64{7}, RequestID: "before"}
	if err := source.node.New(row, "@", nil); err != nil {
		t.Fatal(err)
	}
	zone, err := indexus.ZoneID(row.Collection, row.Location)
	if err != nil {
		t.Fatal(err)
	}
	config.Name = indexus.BASE64.Encode(zone.Bytes())
	config.Storage = filepath.Join(t.TempDir(), "target")
	config.Bootstraps = []indexus.Contact{peer.NewContact(source.node.Name(), map[string]any{"127.0.0.1": nil}, source.port)}
	target := startConfiguredDataNode(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := target.node.CompactCausal(ctx, indexus.CausalCompactionConfig{}); err != nil {
		t.Fatal(err)
	}
	status, err := target.boot.store.ColdStatus()
	if err != nil || status.Drain == nil || !status.Drain.Witnessed {
		t.Fatalf("not witnessed: %+v %v", status, err)
	}
	config = target.boot.config
	target.stop()
	target = startConfiguredDataNode(t, config)
	snapshot, err := target.boot.store.CausalRouting()
	if err != nil || snapshot.Current().Len() != 1 {
		t.Fatalf("restart resurrected vnode: %v", err)
	}
	fresh := *row
	fresh.Id = "after"
	fresh.RequestID = "after"
	fresh.Metrics = []float64{11}
	if err := target.node.New(&fresh, "@", nil); err != nil {
		t.Fatal(err)
	}
	for _, node := range []*runningDataNode{source, target} {
		_, set, err := node.node.Get("Demo000000000000", "@", true, nil, true)
		if err != nil || set == nil || set.Count() != 2 || set.Abelian().Metric(0) != 18 {
			t.Fatalf("retired HTTP route read: %v %v", set, err)
		}
	}
}
