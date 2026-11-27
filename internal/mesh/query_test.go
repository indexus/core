package mesh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type queryInventoryCounter struct {
	Transport
	calls, records atomic.Int64
}

func (c *queryInventoryCounter) Exchange(ctx context.Context, host string, request Request) (Response, error) {
	response, err := c.Transport.Exchange(ctx, host, request)
	if request.Kind == KindInventoryPrefix || request.Kind == "inventory" {
		c.calls.Add(1)
		for _, snapshot := range response.Inventory {
			c.records.Add(int64(len(snapshot.Records)))
		}
	}
	return response, err
}

func TestQueryPrefixDoesNotTransferUnrelatedRecords(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	ctx := context.Background()
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].Sync(ctx, []string{nodes[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	const collection = "0000000000000001"
	var locations [2]string
	snapshot, _ := nodes[0].Store.CausalRouting()
	for _, location := range tree.ChildCandidates(tree.BASE64, "@") {
		zone, _ := tree.NewZoneKey(collection, location)
		owner, _ := snapshot.Current().OwnerName(zone.ID)
		for i, node := range nodes {
			if owner == node.VNode.Name && locations[i] == "" {
				locations[i] = location
			}
		}
	}
	for i, location := range locations {
		if location == "" {
			t.Fatalf("no location for node %d", i)
		}
	}
	if _, _, _, err := nodes[0].Store.ApplyRecord("wanted", storage.Record{Collection: collection, Location: locations[0], ID: "wanted", Metrics: []float64{7}}); err != nil {
		t.Fatal(err)
	}
	const unrelated = 1024
	for i := 0; i < unrelated; i++ {
		id := fmt.Sprintf("unrelated-%d", i)
		if _, _, _, err := nodes[1].Store.ApplyRecord(id, storage.Record{Collection: collection, Location: locations[1], ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	counter := &queryInventoryCounter{Transport: net}
	nodes[0].Peers = counter
	rows, err := nodes[0].QueryPrefix(ctx, collection, locations[0])
	if err != nil || len(rows) != 1 || len(rows[0].Records) != 1 || rows[0].Records[0].ID != "wanted" {
		t.Fatalf("prefix result: %v %v", rows, err)
	}
	// PREFIX-1: the wanted location's subtree is held by nodes[0] alone, so no
	// remote host is asked at all.
	if counter.calls.Load() != 0 || counter.records.Load() != 0 {
		t.Fatalf("unrelated remote records were transferred: RPCs=%d rows=%d", counter.calls.Load(), counter.records.Load())
	}
	// The complete API used by migrations and audits still returns every row.
	rows, err = nodes[0].Query(ctx)
	count := 0
	for _, snapshot := range rows {
		count += len(snapshot.Records)
	}
	if err != nil || count != unrelated+1 {
		t.Fatalf("complete query changed: rows=%d err=%v", count, err)
	}
}

func TestQueryPrefixKeepsCoverageAfterMoveAndRejectsUnavailableShard(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 1)
	source := nodes[0]
	ctx := context.Background()
	row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "row"}
	if _, err := source.Handle(ctx, Request{Kind: "write", RequestID: "seed", Record: &row}); err != nil {
		t.Fatal(err)
	}
	state, _ := source.Store.CausalState()
	store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "moved.db"), source.Self.VNode, "test", 1<<26, testStore)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	holder, _ := store.Endpoint("moved-host")
	target := &Service{Store: store, Self: holder, VNode: source.VNode, Peers: net}
	if err := store.InitCausalReceiver(state.Root, state.Genesis); err != nil {
		t.Fatal(err)
	}
	net.nodes[holder.Host] = target
	if err := target.ReceiveMove(ctx, source.Self.Host); err != nil {
		t.Fatal(err)
	}
	rows, err := source.QueryPrefix(ctx, "Demo000000000000", "a")
	if err != nil || len(rows) != 1 || len(rows[0].Records) != 1 {
		t.Fatalf("stale ingress lost moved subtree: %v %v", rows, err)
	}
	net.setBlocked(holder.Host, true)
	if rows, err := source.QueryPrefix(ctx, "Demo000000000000", "z"); err == nil || len(rows) != 0 {
		t.Fatalf("unavailable shard was presented as complete: %v %v", rows, err)
	}
	if _, _, err := source.Store.Apply("stale-write", row); !errors.Is(err, storage.ErrNotOwner) {
		t.Fatalf("prefix read bypassed retired writer fence: %v", err)
	}
}
