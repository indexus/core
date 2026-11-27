package mesh

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type pageMutationTransport struct {
	Transport
	target     *Service
	mutations  int
	every      bool
	firstPages int
}

func (p *pageMutationTransport) Exchange(ctx context.Context, host string, request Request) (Response, error) {
	out, err := p.Transport.Exchange(ctx, host, request)
	if err == nil && request.Page != nil && request.Page.After == nil && out.Page != nil && !out.Page.Done {
		p.firstPages++
		if p.every || p.mutations == 0 {
			p.mutations++
			_, _, _, err = p.target.Store.ApplyRecord(fmt.Sprintf("between-%d", p.mutations), storage.Record{Collection: "Collection000000", Location: "aa", ID: fmt.Sprintf("between-%d", p.mutations)})
		}
	}
	return out, err
}
func TestPaginatedQueryRetriesWholeReadAfterMutation(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	target, ingress := nodes[0], nodes[1]
	for i := 0; i < 12; i++ {
		if _, _, _, err := target.Store.ApplyRecord(fmt.Sprint(i), storage.Record{Collection: "Collection000000", Location: "aa", ID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	transport := &pageMutationTransport{Transport: net, target: target}
	ingress.Peers = transport
	zone, _ := tree.NewZoneKey("Collection000000", "a")
	rows, err := ingress.query(context.Background(), Request{Kind: KindInventoryPrefix, Zone: zone, Page: &storage.InventoryPageRequest{ByteLimit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, snapshot := range rows {
		for _, row := range snapshot.Records {
			if seen[row.ID] {
				t.Fatal("retry duplicated prior page")
			}
			seen[row.ID] = true
		}
	}
	if len(rows) != 1 || len(seen) != 13 || transport.mutations != 1 || transport.firstPages != 2 {
		t.Fatalf("retry did not replace whole read: snapshots=%d rows=%d changes=%d attempts=%d", len(rows), len(seen), transport.mutations, transport.firstPages)
	}
}
func TestPaginatedQueryBoundsRetriesAndNeverReturnsPartialResult(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 2)
	target, ingress := nodes[0], nodes[1]
	for i := 0; i < 3; i++ {
		if _, _, _, err := target.Store.ApplyRecord(fmt.Sprint(i), storage.Record{Collection: "Collection000000", Location: "aa", ID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	transport := &pageMutationTransport{Transport: net, target: target, every: true}
	ingress.Peers = transport
	rows, err := ingress.query(context.Background(), Request{Kind: "inventory", Page: &storage.InventoryPageRequest{ByteLimit: 1}})
	if !errors.Is(err, storage.ErrReadChanged) || rows != nil || transport.firstPages != 8 {
		t.Fatalf("unstable read returned success/unbounded retries: rows=%d err=%v attempts=%d", len(rows), err, transport.firstPages)
	}
}
func TestPaginatedQueryHonorsAlreadyCancelledContext(t *testing.T) {
	t.Parallel()
	_, nodes := meshFixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rows, err := nodes[0].QueryPrefix(ctx, "Collection000000", "a"); !errors.Is(err, context.Canceled) || rows != nil {
		t.Fatalf("cancelled read: %v", err)
	}
}

func TestPaginatedQueryRejectsHostReusedByAnotherPhysicalHolder(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 3)
	// All three disks know genesis. The discovery host now answers from a
	// different empty disk, which has no installed grant for the expected holder.
	net.nodes[nodes[0].Self.Host] = nodes[1]
	rows, err := nodes[2].QueryPrefix(context.Background(), "Collection000000", "a")
	if !errors.Is(err, storage.ErrNotReady) || rows != nil {
		t.Fatalf("wrong physical disk certified an empty complete result: rows=%d err=%v", len(rows), err)
	}
}

func TestQueryBudgetRejectsBeforeRetainingWholeInventory(t *testing.T) {
	t.Parallel()
	_, nodes := meshFixture(t, 1)
	for i := 0; i < 12; i++ {
		_, _, _, err := nodes[0].Store.ApplyRecord(fmt.Sprint(i), storage.Record{Collection: "Collection000000", Location: "aa", ID: fmt.Sprint(i), Payload: []byte(`{"value":"payload"}`)})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, limits := range [][2]int{{1, 1 << 20}, {100, 128}} {
		rows, err := nodes[0].QueryPrefixLimited(context.Background(), "Collection000000", "a", limits[0], limits[1])
		if !errors.Is(err, ErrQueryBudget) || rows != nil {
			t.Fatalf("budget returned partial result: %v %v", rows, err)
		}
	}
	rows, err := nodes[0].QueryPrefixLimited(context.Background(), "Collection000000", "a", 12, 1<<20)
	if err != nil || len(rows) != 1 || len(rows[0].Records) != 12 {
		t.Fatalf("bounded success: %v %v", rows, err)
	}
}
func TestQueryBudgetIncludesTombstonesAndEmptyZoneOverhead(t *testing.T) {
	t.Parallel()
	b := &queryBudget{maxRecords: 1, maxBytes: 256}
	rows := []storage.Snapshot{{Records: []storage.Record{{Deleted: true, Payload: make([]byte, 1024)}}}}
	if err := b.accept(rows); !errors.Is(err, ErrQueryBudget) {
		t.Fatal(err)
	}
	b = &queryBudget{maxRecords: 1, maxBytes: 256}
	if err := b.accept(make([]storage.Snapshot, 3)); !errors.Is(err, ErrQueryBudget) {
		t.Fatal(err)
	}
}

func TestMetricsQueryProjectsContentAcrossRemotePages(t *testing.T) {
	t.Parallel()
	_, nodes := meshFixture(t, 2)
	for i := 0; i < 8; i++ {
		_, _, _, err := nodes[0].Store.ApplyRecord(fmt.Sprint(i), storage.Record{Collection: "Demo000000000000", Location: "aa", ID: fmt.Sprint(i), Metrics: []float64{3}, Payload: []byte(`{"content":"kept on disk"}`)})
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := nodes[1].QueryPrefixMetrics(context.Background(), "Demo000000000000", "a")
	if err != nil || len(rows) != 1 || len(rows[0].Records) != 8 {
		t.Fatalf("remote metric projection: %v %v", rows, err)
	}
	for _, r := range rows[0].Records {
		if len(r.Payload) != 0 || r.Metrics[0] != 3 {
			t.Fatal(r)
		}
	}
	full, err := nodes[1].QueryPrefix(context.Background(), "Demo000000000000", "a")
	if err != nil || len(full[0].Records[0].Payload) == 0 {
		t.Fatalf("full content lost: %v", err)
	}
}
