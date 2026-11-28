package indexus

import (
	"context"
	"fmt"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// Observe the data actually carried between the two nodes, independently of
// how many child keys the facade ultimately returns.
type childInventoryProbe struct {
	mesh.Transport
	deepRows atomic.Int64
}

func (p *childInventoryProbe) Exchange(ctx context.Context, host string, req mesh.Request) (mesh.Response, error) {
	out, err := p.Transport.Exchange(ctx, host, req)
	if err == nil && req.Kind == mesh.KindInventoryPrefix && req.Page != nil && req.Page.MetricsOnly {
		for _, snap := range out.Inventory {
			for _, r := range snap.Records {
				if r.Location == "012345" && !r.Deleted {
					p.deepRows.Add(1)
				}
			}
		}
	}
	return out, err
}

func TestFreshChildDiscoveryDuringHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const collection = "0000000000000001"
	retained, err := NewZoneKey(collection, "0zABCDEFG")
	if err != nil {
		t.Fatal(err)
	}
	root := VNodeDescriptor{ID: retained.ID}
	root.Name = BASE64.Encode(root.ID.Bytes())
	moved, err := NewZoneKey(collection, "00")
	if err != nil {
		t.Fatal(err)
	}
	target := VNodeDescriptor{ID: moved.ID}
	target.Name = BASE64.Encode(target.ID.Bytes())
	f := newReadPhaseFixture(t, []VNodeDescriptor{root, target}, false, false)
	probe := &childInventoryProbe{Transport: f.net}
	for _, svc := range f.services {
		svc.Peers = probe
	}
	row := func(loc, id string) storage.Record {
		return storage.Record{Collection: collection, Location: loc, ID: id, Metrics: []float64{1}}
	}
	rows := []storage.Record{row("0", "self"), row("00", "to-remove"), row("01", "intermediate"), row("0zABCDEFG", "compressed")}
	const deep = 96
	for i := range deep {
		rows = append(rows, row("012345", fmt.Sprint(i)))
	}
	if err := f.stores[0].SeedLocalRecords(rows); err != nil {
		t.Fatal(err)
	}
	var exercised bool
	var readRows int64
	f.net.before = func(req mesh.Request) error {
		if req.Kind != "install" || exercised {
			return nil
		}
		exercised = true
		deleted := row("00", "to-remove")
		deleted.Deleted = true
		ack, err := f.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: "tombstone-child", Record: &deleted})
		if err != nil {
			return err
		}
		if ack.AppliedBy != f.services[1].Self {
			return fmt.Errorf("new holder did not ACK tombstone")
		}
		local, err := f.stores[0].ZoneChildren(collection, "0")
		if err != nil {
			return err
		}
		if local[1+BASE64.IndexOf('0')].Count != 1 {
			return fmt.Errorf("expected the pre-handoff source copy at child 0; got %d", local[1+BASE64.IndexOf('0')].Count)
		}
		receiver, err := f.stores[1].ZoneChildren(collection, "0")
		if err != nil {
			return err
		}
		if receiver[1+BASE64.IndexOf('z')].Count != 0 {
			return fmt.Errorf("receiver has the source-owned child z")
		}
		srcSeen, _, err := f.stores[0].ObserveChildPresence(ctx, collection, "0")
		if err != nil {
			return err
		}
		dstSeen, dstUnknown, err := f.stores[1].ObserveChildPresence(ctx, collection, "0")
		if err != nil {
			return err
		}
		if srcSeen&(uint64(1)<<BASE64.IndexOf('0')) == 0 || (dstSeen|dstUnknown)&(uint64(1)<<BASE64.IndexOf('z')) != 0 {
			return fmt.Errorf("expected stale source witness at 0 and no target copy at z; got source %x, target %x/%x", srcSeen, dstSeen, dstUnknown)
		}
		// A direct record at the intermediate prefix shares its child's summary
		// with deeper records. A unique deep location may produce a shortened key.
		want := map[string]int{"0": 1, "01": 1 + deep, "0zABC": 1}
		start := probe.deepRows.Load()
		for _, n := range f.nodes {
			result, err := n.ReadContext(ctx, collection, "0", ReadOptions{Mode: ReadFresh})
			if err != nil {
				return err
			}
			got := map[string]int{}
			for k, v := range result.Set.List() {
				got[k] = v.Count()
			}
			if result.Source != "inventory" || !maps.Equal(got, want) {
				return fmt.Errorf("fresh inventory %q returned %v, want %v", result.Source, got, want)
			}
		}
		readRows = probe.deepRows.Load() - start
		return nil
	}
	if err := f.services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if !exercised {
		t.Fatal("regional install cut not reached")
	}
	t.Logf("3 result keys per entry point; %d deep inventory rows across two fresh reads", readRows)
}
