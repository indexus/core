package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/peer"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/wire"
)

func TestCausalRuntimeHTTPJoinStaleRouteAndRestart(t *testing.T) {
	config := Config{CreateCluster: true, Name: "AAAAAAAAAAAAAAAB", Storage: filepath.Join(t.TempDir(), "source"), Advertise: []string{"127.0.0.1"}, ClusterID: "integration", ClusterCapacity: 1 << 26}
	source := startConfiguredDataNode(t, config)
	item := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "record", Metrics: []float64{7}, RequestID: "put-1"}
	if err := source.node.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	config.Name = indexus.BASE64.Encode(zone.Bytes())
	config.Storage = filepath.Join(t.TempDir(), "target")
	config.Bootstraps = []indexus.Contact{peer.NewContact(source.node.Name(), map[string]any{"127.0.0.1": nil}, source.port)}
	target := startConfiguredDataNode(t, config)
	for _, node := range []*runningDataNode{source, target} {
		_, set, err := node.node.Get("Demo000000000000", "@", true, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if set.Count() != 1 {
			t.Fatalf("lost row: %d", set.Count())
		}
	}
	deletion := *item
	deletion.RequestID = "delete-2"
	if err := source.node.Delete(&deletion, "@", nil); err != nil {
		t.Fatal(err)
	}
	config = target.boot.config
	target.stop()
	target = startConfiguredDataNode(t, config)
	if err := source.node.New(item, "@", nil); err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	_, set, err := target.node.Get("Demo000000000000", "@", true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if set.Count() != 0 {
		t.Fatal("old retry resurrected delete")
	}
}

func TestCausalStartupRefusesReplacementDatabase(t *testing.T) {
	config := Config{CreateCluster: true, Name: "AAAAAAAAAAAAAAAB", Storage: filepath.Join(t.TempDir(), "pinned"), Advertise: []string{"127.0.0.1"}, ClusterID: "integration", ClusterCapacity: 1 << 26}
	running := startConfiguredDataNode(t, config)
	config = running.boot.config
	running.stop()
	if err := os.RemoveAll(storage.RecordsDir(config.Storage + ".causal.db")); err != nil {
		t.Fatal(err)
	}
	replacement, err := storage.Open(config.Storage+".causal.db", config.Name, config.ClusterID, config.ClusterCapacity)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	boot := &clusterBoot{config: config, store: replacement}
	if err := boot.startMesh(context.Background(), nil); !errors.Is(err, storage.ErrMissingData) {
		t.Fatalf("replacement minted a genesis: %v", err)
	}
}

func TestCausalAggregateRefreshAcrossChildrenAndRestart(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	zone, err := indexus.ZoneID("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	target := startDataNode(t, indexus.BASE64.Encode(zone.Bytes()), source)
	nodes := []*runningDataNode{source, target}
	check := func(count int, sum float64) {
		t.Helper()
		for range 2 {
			for _, node := range nodes {
				if err := node.node.Update(); err != nil {
					t.Fatal(err)
				}
			}
		}
		aggregates := ownerAggregates(t, nodes, "Demo000000000000", []string{"@", "a", "aa", "ab"})
		for _, parent := range []string{"@", "a"} {
			a := aggregates[parent]
			if a == nil || a.Count() != count || a.Metric(0) != sum {
				t.Fatalf("parent %s: %v, want count=%d sum=%g", parent, a, count, sum)
			}
		}
		children := wire.NewAbelian(0, nil)
		children.Sum(aggregates["aa"])
		children.Sum(aggregates["ab"])
		if children.Count() != count || children.Metric(0) != sum {
			t.Fatalf("children: %v", children)
		}
	}
	a := &indexus.Item{Collection: "Demo000000000000", Location: "aa", Id: "one", Metrics: []float64{7}, RequestID: "one-v1"}
	b := &indexus.Item{Collection: "Demo000000000000", Location: "ab", Id: "two", Metrics: []float64{11}, RequestID: "two-v1"}
	for _, item := range []*indexus.Item{a, b} {
		body, err := json.Marshal(map[string]any{"item": item, "root": "@"})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		source.server.New(response, httptest.NewRequest(http.MethodPost, "/item", bytes.NewReader(body)))
		if response.Code != http.StatusCreated {
			t.Fatalf("write: %d %s", response.Code, response.Body.String())
		}
	}
	check(2, 18)
	a.Metrics, a.RequestID = []float64{19}, "one-v2"
	if err := source.node.New(a, "@", nil); err != nil {
		t.Fatal(err)
	}
	check(2, 30)
	b.RequestID = "two-delete"
	if err := source.node.Delete(b, "@", nil); err != nil {
		t.Fatal(err)
	}
	check(1, 19)
	config := target.boot.config
	target.stop()
	if err := source.node.Update(); err == nil {
		t.Fatal("partial inventory published while child host unavailable")
	}
	if retained, err := source.node.GetAggregates("Demo000000000000", []string{"@"}); err != nil {
		t.Fatal(err)
	} else if a := retained["@"]; a != nil && (a.Count() != 1 || a.Metric(0) != 19) {
		t.Fatalf("partition erased parent: %v", a)
	}
	target = startConfiguredDataNode(t, config)
	nodes = []*runningDataNode{source, target}
	check(1, 19)
	a.RequestID = "one-delete"
	if err := source.node.Delete(a, "@", nil); err != nil {
		t.Fatal(err)
	}
	check(0, 0)
}

func ownerAggregates(t *testing.T, nodes []*runningDataNode, collection string, locations []string) map[string]*indexus.Abelian {
	t.Helper()
	out := make(map[string]*indexus.Abelian, len(locations))
	for _, location := range locations {
		found := 0
		for _, node := range nodes {
			aggregates, err := node.node.GetAggregates(collection, []string{location})
			if err != nil {
				t.Fatal(err)
			}
			if value, ok := aggregates[location]; ok {
				out[location] = value
				found++
			}
		}
		if found != 1 {
			t.Fatalf("%s/%s answered by %d owners", collection, location, found)
		}
	}
	return out
}

func TestMalformedMoveEndpointFailsBeforeBootstrapRetry(t *testing.T) {
	_, err := configureCluster(Config{Storage: filepath.Join(t.TempDir(), "bad"), Advertise: []string{"127.0.0.1"}, MoveFrom: "127.0.0.1:21030"}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("malformed endpoint entered bootstrap retry loop")
	}
}
