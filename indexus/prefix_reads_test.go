package indexus

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

type prefixReadTransport struct {
	next                         mesh.Transport
	prefixCalls, fullCalls, rows atomic.Int64
}

func (p *prefixReadTransport) Exchange(ctx context.Context, host string, request mesh.Request) (mesh.Response, error) {
	out, err := p.next.Exchange(ctx, host, request)
	if request.Kind == mesh.KindInventoryPrefix {
		p.prefixCalls.Add(1)
	} else if request.Kind == "inventory" {
		p.fullCalls.Add(1)
	}
	if request.Kind == mesh.KindInventoryPrefix || request.Kind == "inventory" {
		for _, snapshot := range out.Inventory {
			p.rows.Add(int64(len(snapshot.Records)))
		}
	}
	return out, err
}

func TestNodePrefixRefreshAndRemoteDeepReadUseFilteredInventory(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
	if err := f.nodes[1].JoinCausalCluster(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	for _, node := range f.nodes {
		if err := node.meshRefreshView(); err != nil {
			t.Fatal(err)
		}
	}
	const collection = "0000000000000001"
	var wanted, unrelated string
	for _, location := range ChildCandidates(BASE64, "@") {
		zone, _ := NewZoneKey(collection, location)
		owner, _ := f.nodes[0].topologyView().OwnerName(zone.ID)
		if owner == descriptors[0].Name && wanted == "" {
			wanted = location
		}
		if owner == descriptors[1].Name && unrelated == "" {
			unrelated = location
		}
	}
	if wanted == "" || unrelated == "" {
		t.Fatal("fixture does not cover both holders")
	}
	for i, location := range []string{wanted, unrelated} {
		if _, _, _, err := f.stores[i].ApplyRecord(location, storage.Record{Collection: collection, Location: location, ID: "row", Metrics: []float64{7}}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("local-summary-referral", func(t *testing.T) {
		counter := &prefixReadTransport{next: f.net}
		f.services[1].Peers = counter
		defer func() { f.services[1].Peers = f.net }()
		result, err := f.nodes[1].ReadContext(ctx, collection, wanted, ReadOptions{Mode: ReadLocal})
		if err != nil || result.Set != nil || result.Contact == nil || result.Contact.Name() != descriptors[0].Name {
			t.Fatalf("local read should give the owner: %+v, %v", result, err)
		}
		if counter.fullCalls.Load() != 0 || counter.prefixCalls.Load() != 0 {
			t.Fatal("local read called the mesh inventory")
		}
	})

	for _, tc := range []struct {
		name        string
		ingress     int
		refresh     bool
		remoteCalls int64
		remoteRows  int64
	}{
		// PREFIX-1: the prefix's subtree is held by the ingress alone, so
		// the read asks no other host.
		{"refresh-at-owner", 0, true, 0, 0},
		{"deep-from-other-holder", 1, false, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &prefixReadTransport{next: f.net}
			f.services[tc.ingress].Peers = counter
			// Both paths use durable prefix inventory.
			_, set, err := f.nodes[tc.ingress].Get(collection, wanted, true, nil, tc.refresh)
			if err != nil || set == nil || set.Count() != 1 || set.Abelian().Metric(0) != 7 {
				t.Fatalf("prefix read: %v %v", set, err)
			}
			if counter.fullCalls.Load() != 0 || counter.prefixCalls.Load() != tc.remoteCalls || counter.rows.Load() != tc.remoteRows {
				t.Fatalf("unfiltered read: full=%d prefix=%d remote rows=%d", counter.fullCalls.Load(), counter.prefixCalls.Load(), counter.rows.Load())
			}
			f.services[tc.ingress].Peers = f.net
		})
	}
}
