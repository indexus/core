package meshhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

func TestInventoryQueryTransfersMoreThan32MiBInBoundedHTTPPages(t *testing.T) {
	var receiver *mesh.Service
	var totalBytes, maxBytes, pages atomic.Int64
	handler := &Handler{Mesh: func(ctx context.Context, request mesh.Request) (mesh.Response, error) {
		out, err := receiver.Handle(ctx, request)
		if request.Page != nil && err == nil {
			raw, e := json.Marshal(meshEnvelope{Response: out})
			if e != nil {
				return mesh.Response{}, e
			}
			size := int64(len(raw))
			totalBytes.Add(size)
			pages.Add(1)
			for {
				old := maxBytes.Load()
				if old >= size || maxBytes.CompareAndSwap(old, size) {
					break
				}
			}
		}
		return out, err
	}}
	server := httptest.NewServer(handler)
	defer server.Close()
	open := func(name string) *storage.Store {
		store, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), name, "paged-http", 1<<30, storage.Options{NoSync: true, BatchWrites: true, BatchSize: 32})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		return store
	}
	disk := open("owner")
	holder, err := disk.Endpoint(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	root := indexus.VNodeDescriptor{Name: "owner", ID: indexus.ID{1}}
	genesis := causal.Genesis{Cluster: "paged-http", Holder: holder}
	if err = disk.InitCausal(root, genesis); err != nil {
		t.Fatal(err)
	}
	receiver = &mesh.Service{Store: disk, Self: holder, VNode: root, Peers: &Client{}}
	// Large payloads produce a real >32MiB inventory with only 256 logical rows.
	// Inserts share the production bounded writer and group commits.
	const records = 256
	// A row of 14000*13 bytes of JSON: as large as 14000 metrics were.
	payload := json.RawMessage(`"` + strings.Repeat("x", 182000) + `"`)
	failures := make(chan error, records)
	var wait sync.WaitGroup
	jobs := make(chan int)
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := range jobs {
				_, _, _, err := disk.ApplyRecord(fmt.Sprint(i), storage.Record{Collection: "Collection000000", Location: "aa", ID: fmt.Sprintf("%08d", i), Payload: payload})
				if err != nil {
					failures <- err
				}
			}
		}()
	}
	for i := 0; i < records; i++ {
		jobs <- i
	}
	close(jobs)
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	ingressDisk := open("ingress")
	ingressHolder, _ := ingressDisk.Endpoint("ingress")
	if err = ingressDisk.InitCausal(root, genesis); err != nil {
		t.Fatal(err)
	}
	ingress := &mesh.Service{Store: ingressDisk, Self: ingressHolder, VNode: indexus.VNodeDescriptor{Name: "ingress", ID: indexus.ID{200}}, Peers: &Client{}}
	rows, err := ingress.QueryPrefix(context.Background(), "Collection000000", "a")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, snapshot := range rows {
		for _, row := range snapshot.Records {
			if seen[row.ID] || !bytes.Equal(row.Payload, payload) {
				t.Fatal("paged records changed or repeated")
			}
			seen[row.ID] = true
		}
	}
	if len(rows) != 1 || len(seen) != records || totalBytes.Load() <= maxBody || pages.Load() < 2 || maxBytes.Load() > storage.DefaultInventoryPageBytes+64*1024 {
		t.Fatalf("unbounded/incomplete transfer: rows=%d bytes=%d pages=%d max=%d", len(seen), totalBytes.Load(), pages.Load(), maxBytes.Load())
	}
	t.Logf("real HTTP inventory: records=%d bytes=%d pages=%d largestResponse=%d", len(seen), totalBytes.Load(), pages.Load(), maxBytes.Load())
	// The global Query wrapper also uses page requests rather than one giant RPC.
	totalBytes.Store(0)
	pages.Store(0)
	all, err := ingress.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || len(all[0].Records) != records || pages.Load() < 2 {
		t.Fatal("global Query bypassed pagination")
	}
}

func TestInventoryHTTPAllowsOneRowBeyondSoftPageBudget(t *testing.T) {
	// A valid row can be larger than 1MiB without exceeding the hard transport
	// limit. The default page budget is not an item-size restriction.
	disk, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), "owner", "single-large", 1<<27, storage.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	holder, _ := disk.Endpoint("owner")
	root := indexus.VNodeDescriptor{Name: "owner", ID: indexus.ID{1}}
	genesis := causal.Genesis{Cluster: "single-large", Holder: holder}
	if err = disk.InitCausal(root, genesis); err != nil {
		t.Fatal(err)
	}
	// A row of 100000*13 bytes of JSON: as large as 100000 metrics were.
	payload := json.RawMessage(`"` + strings.Repeat("x", 1300000) + `"`)
	if _, _, _, err = disk.ApplyRecord("wide", storage.Record{Collection: "Collection000000", Location: "aa", ID: "wide", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	service := &mesh.Service{Store: disk, Self: holder, VNode: root}
	server := httptest.NewServer(&Handler{Mesh: service.Handle})
	defer server.Close()
	zone, _ := indexus.NewZoneKey("Collection000000", "a")
	out, err := (&Client{}).Exchange(context.Background(), server.URL, mesh.Request{Kind: mesh.KindInventoryPrefix, Zone: zone, Page: &storage.InventoryPageRequest{}})
	if err != nil || out.Page == nil || !out.Page.Done || len(out.Inventory) != 1 || len(out.Inventory[0].Records) != 1 {
		t.Fatalf("soft budget rejected a legal item: %v", err)
	}
	raw, _ := json.Marshal(out)
	if len(raw) <= storage.DefaultInventoryPageBytes || len(raw) >= maxBody {
		t.Fatalf("fixture size=%d", len(raw))
	}
}

func TestInventoryHTTPReportsOversizeItemExplicitly(t *testing.T) {
	server := httptest.NewServer(&Handler{Mesh: func(context.Context, mesh.Request) (mesh.Response, error) {
		return mesh.Response{Page: &storage.InventoryPage{Done: true}, Inventory: []storage.Snapshot{{Records: []storage.Record{{ID: strings.Repeat("x", maxBody)}}}}}, nil
	}})
	defer server.Close()
	out, err := (&Client{}).Exchange(context.Background(), server.URL, mesh.Request{Kind: "inventory", Page: &storage.InventoryPageRequest{}})
	if !errors.Is(err, storage.ErrInventoryItemTooLarge) || len(out.Inventory) != 0 {
		t.Fatalf("oversize reply became truncation/partial success: %v", err)
	}
}

func TestInventoryReadChangedRemainsRetryableThroughHTTP(t *testing.T) {
	server := httptest.NewServer(&Handler{Mesh: func(context.Context, mesh.Request) (mesh.Response, error) {
		return mesh.Response{}, storage.ErrReadChanged
	}})
	defer server.Close()
	_, err := (&Client{}).Exchange(context.Background(), server.URL, mesh.Request{Kind: "inventory", Page: &storage.InventoryPageRequest{}})
	if !errors.Is(err, storage.ErrReadChanged) || !errors.Is(err, storage.ErrNotReady) {
		t.Fatalf("read-changed lost retry classification: %v", err)
	}
	recorder := httptest.NewRecorder()
	(&Handler{Mesh: func(context.Context, mesh.Request) (mesh.Response, error) {
		return mesh.Response{}, storage.ErrInventoryItemTooLarge
	}}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/cluster/mesh", strings.NewReader(`{"Kind":"inventory","Page":{}}`)))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize HTTP status=%d", recorder.Code)
	}
}

func TestReceiverClosedPreservesClassificationThroughHTTP(t *testing.T) {
	server := httptest.NewServer(&Handler{Mesh: func(context.Context, mesh.Request) (mesh.Response, error) {
		return mesh.Response{}, storage.ErrReceiverClosed
	}})
	defer server.Close()
	_, err := (&Client{}).Exchange(context.Background(), server.URL, mesh.Request{Kind: "stage"})
	if !errors.Is(err, storage.ErrReceiverClosed) || !errors.Is(err, storage.ErrNotReady) {
		t.Fatalf("receiver-closed lost classification over HTTP: %v", err)
	}
}

func TestInventoryHTTPMutationBetweenPagesRetriesExactWholeQuery(t *testing.T) {
	var receiver *mesh.Service
	var changed atomic.Bool
	var initialPages atomic.Int64
	var disk *storage.Store
	server := httptest.NewServer(&Handler{Mesh: func(ctx context.Context, request mesh.Request) (mesh.Response, error) {
		out, err := receiver.Handle(ctx, request)
		if err == nil && request.Page != nil && request.Page.After == nil && out.Page != nil && !out.Page.Done {
			initialPages.Add(1)
			if changed.CompareAndSwap(false, true) {
				_, _, _, err = disk.ApplyRecord("between-pages", storage.Record{Collection: "Collection000000", Location: "aa", ID: "new"})
			}
		}
		return out, err
	}})
	defer server.Close()
	var err error
	disk, err = storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), "owner", "http-retry", 1<<27, storage.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	holder, _ := disk.Endpoint(server.URL)
	root := indexus.VNodeDescriptor{Name: "owner", ID: indexus.ID{1}}
	genesis := causal.Genesis{Cluster: "http-retry", Holder: holder}
	if err = disk.InitCausal(root, genesis); err != nil {
		t.Fatal(err)
	}
	receiver = &mesh.Service{Store: disk, Self: holder, VNode: root}
	// A row of 100000*13 bytes of JSON: as large as 100000 metrics were.
	payload := json.RawMessage(`"` + strings.Repeat("x", 1300000) + `"`)
	for i := 0; i < 3; i++ {
		if _, _, _, err = disk.ApplyRecord(fmt.Sprint(i), storage.Record{Collection: "Collection000000", Location: "aa", ID: fmt.Sprint(i), Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	ingressDisk, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "data.db"), "ingress", "http-retry", 1<<27, storage.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ingressDisk.Close()
	ingressHolder, _ := ingressDisk.Endpoint("ingress")
	if err = ingressDisk.InitCausal(root, genesis); err != nil {
		t.Fatal(err)
	}
	ingress := &mesh.Service{Store: ingressDisk, Self: ingressHolder, VNode: indexus.VNodeDescriptor{Name: "ingress", ID: indexus.ID{200}}, Peers: &Client{}}
	rows, err := ingress.QueryPrefix(context.Background(), "Collection000000", "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Records) != 4 || initialPages.Load() != 2 {
		t.Fatalf("HTTP retry was partial/duplicated: snapshots=%d attempts=%d err=%v", len(rows), initialPages.Load(), err)
	}
	seen := map[string]bool{}
	for _, record := range rows[0].Records {
		if seen[record.ID] {
			t.Fatal("duplicate from abandoned first page")
		}
		seen[record.ID] = true
	}
	if !seen["new"] {
		t.Fatal("retried query omitted the concurrent insertion")
	}
}
